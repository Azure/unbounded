package netboot

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	qcow2reader "github.com/lima-vm/go-qcow2reader"
	v1alpha3 "github.com/project-unbounded/unbounded-kube/api/v1alpha3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type ImageReconciler struct {
	Client       client.Client
	CacheDir     string
	MaxDownloads int

	semOnce sync.Once
	sem     chan struct{}
}

func (r *ImageReconciler) initSem() {
	r.semOnce.Do(func() {
		n := r.MaxDownloads
		if n <= 0 {
			n = 8
		}

		r.sem = make(chan struct{}, n)
	})
}

func (r *ImageReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.initSem()
	return ctrl.NewControllerManagedBy(mgr).For(&v1alpha3.Image{}).Complete(r)
}

func (r *ImageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.initSem()

	logger := log.FromContext(ctx)

	var image v1alpha3.Image
	if err := r.Client.Get(ctx, req.NamespacedName, &image); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	if err := os.MkdirAll(filepath.Join(r.CacheDir, "sha256"), 0o755); err != nil {
		return ctrl.Result{}, fmt.Errorf("creating cache dir: %w", err)
	}

	type downloadResult struct {
		path       string
		networkErr bool
		fatalErr   error
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []downloadResult
	)

	for _, file := range image.Spec.Files {
		if file.HTTP == nil {
			continue
		}

		file := file // capture loop variable

		wg.Add(1)

		go func() {
			defer wg.Done()

			select {
			case r.sem <- struct{}{}:
				defer func() { <-r.sem }()
			case <-ctx.Done():
				return
			}

			if file.HTTP.Convert != "" {
				switch file.HTTP.Convert {
				case "UnpackQcow2":
				default:
					mu.Lock()

					results = append(results, downloadResult{path: file.Path, fatalErr: fmt.Errorf("unsupported convert value %q", file.HTTP.Convert)})

					mu.Unlock()

					return
				}
			}

			destPath := cachePath(r.CacheDir, file.HTTP.SHA256, file.HTTP.Convert)
			if _, err := os.Stat(destPath); err == nil {
				return // already cached
			}

			logger.Info("downloading file", "path", file.Path, "url", file.HTTP.URL)

			var err error
			if file.HTTP.Convert != "" {
				err = downloadAndConvertFile(ctx, destPath, file.HTTP.URL, file.HTTP.SHA256, file.HTTP.Convert)
			} else {
				err = downloadFile(ctx, destPath, file.HTTP.URL, file.HTTP.SHA256)
			}

			if err != nil {
				mu.Lock()

				if strings.Contains(err.Error(), "checksum mismatch") {
					logger.Error(err, "checksum mismatch, not requeueing", "path", file.Path)
				} else {
					logger.Error(err, "download failed", "path", file.Path)
					results = append(results, downloadResult{path: file.Path, networkErr: true})
				}

				mu.Unlock()
			} else {
				logger.Info("file cached", "path", file.Path)
			}
		}()
	}

	wg.Wait()

	var networkErr bool

	for _, res := range results {
		if res.fatalErr != nil {
			return ctrl.Result{}, res.fatalErr
		}

		if res.networkErr {
			networkErr = true
		}
	}

	if networkErr {
		return ctrl.Result{RequeueAfter: 30_000_000_000}, nil // 30s
	}

	return ctrl.Result{}, nil
}

func downloadFile(ctx context.Context, destPath, url, expectedSHA256 string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck // Best-effort close of HTTP response body.

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading %s: status %d", url, resp.StatusCode)
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(destPath), ".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}

	tmpPath := tmpFile.Name()

	defer func() {
		tmpFile.Close()    //nolint:errcheck // Best-effort close of temp file on cleanup.
		os.Remove(tmpPath) //nolint:errcheck // Best-effort removal of temp file.
	}()

	hasher := sha256.New()
	if _, err := io.Copy(tmpFile, io.TeeReader(resp.Body, hasher)); err != nil {
		return fmt.Errorf("writing temp file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}

	actualSHA256 := hex.EncodeToString(hasher.Sum(nil))
	if actualSHA256 != expectedSHA256 {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s", url, actualSHA256, expectedSHA256)
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("renaming temp file: %w", err)
	}

	return nil
}

// downloadAndConvertFile downloads a source file, verifies its SHA256 against
// expectedSHA256, converts it according to the convert method, and atomically
// writes the result to destPath.
func downloadAndConvertFile(ctx context.Context, destPath, url, expectedSHA256, convert string) error {
	dir := filepath.Dir(destPath)

	// Download to a temp file
	tmpSrc, err := os.CreateTemp(dir, ".tmp-src-*")
	if err != nil {
		return fmt.Errorf("creating temp source file: %w", err)
	}

	tmpSrcPath := tmpSrc.Name()
	defer os.Remove(tmpSrcPath) //nolint:errcheck // Best-effort removal of temp source file.

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		tmpSrc.Close() //nolint:errcheck // Best-effort close before returning error.
		return fmt.Errorf("creating request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		tmpSrc.Close() //nolint:errcheck // Best-effort close before returning error.
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck // Best-effort close of HTTP response body.

	if resp.StatusCode != http.StatusOK {
		tmpSrc.Close() //nolint:errcheck // Best-effort close before returning error.
		return fmt.Errorf("downloading %s: status %d", url, resp.StatusCode)
	}

	hasher := sha256.New()
	if _, err := io.Copy(tmpSrc, io.TeeReader(resp.Body, hasher)); err != nil {
		tmpSrc.Close() //nolint:errcheck // Best-effort close before returning error.
		return fmt.Errorf("writing source temp file: %w", err)
	}

	if err := tmpSrc.Close(); err != nil {
		return fmt.Errorf("closing source temp file: %w", err)
	}

	actualSHA256 := hex.EncodeToString(hasher.Sum(nil))
	if actualSHA256 != expectedSHA256 {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s", url, actualSHA256, expectedSHA256)
	}

	// Convert source to target encoding
	return convertFile(tmpSrcPath, destPath, convert)
}

// convertFile reads srcPath and writes destPath according to the convert
// method. "UnpackQcow2" reads a qcow2 image and writes raw gzip-compressed
// data.
func convertFile(srcPath, destPath, convert string) error {
	if convert != "UnpackQcow2" {
		return fmt.Errorf("unsupported convert method %q", convert)
	}

	srcFile, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("opening source: %w", err)
	}
	defer srcFile.Close() //nolint:errcheck // Best-effort close of source file.

	img, err := qcow2reader.Open(srcFile)
	if err != nil {
		return fmt.Errorf("opening qcow2: %w", err)
	}
	defer img.Close() //nolint:errcheck // Best-effort close of qcow2 image.

	size := img.Size()
	if size < 0 {
		return fmt.Errorf("qcow2 image has unknown size")
	}

	tmpDst, err := os.CreateTemp(filepath.Dir(destPath), ".tmp-dst-*")
	if err != nil {
		return fmt.Errorf("creating temp dest file: %w", err)
	}

	tmpDstPath := tmpDst.Name()
	defer os.Remove(tmpDstPath) //nolint:errcheck // Best-effort removal of temp dest file.

	gw := gzip.NewWriter(tmpDst)

	r := io.NewSectionReader(img, 0, size)
	if _, err := io.Copy(gw, r); err != nil {
		gw.Close()     //nolint:errcheck // Best-effort close before returning error.
		tmpDst.Close() //nolint:errcheck // Best-effort close before returning error.

		return fmt.Errorf("streaming conversion: %w", err)
	}

	if err := gw.Close(); err != nil {
		tmpDst.Close() //nolint:errcheck // Best-effort close before returning error.
		return fmt.Errorf("closing gzip writer: %w", err)
	}

	if err := tmpDst.Close(); err != nil {
		return fmt.Errorf("closing temp dest file: %w", err)
	}

	if err := os.Rename(tmpDstPath, destPath); err != nil {
		return fmt.Errorf("renaming converted file: %w", err)
	}

	return nil
}
