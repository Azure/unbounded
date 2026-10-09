package main

import (
	"context"
	"crypto/sha256"
	"io"
	"os"

	"github.com/Azure/unbounded/pkg/racersdk"
)

func main() {
	if err := runGet(); err != nil {
		panic(err)
	}
}

func runGet() error {
	client, err := racersdk.NewClient(racersdk.ClientConfig{Cache: "racer-demo"})
	if err != nil {
		return err
	}
	defer client.Close()

	obj, err := client.Get(context.TODO(), racersdk.Request{
		Key: sha256.Sum256([]byte("0")),
	})
	if err != nil {
		return err
	}
	defer obj.Close()

	_, err = io.Copy(os.Stdout, obj)

	return err
}
