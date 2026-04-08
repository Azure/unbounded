package machina

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/project-unbounded/unbounded-kube/cmd/machina/machina/controller"
	"github.com/project-unbounded/unbounded-kube/internal/version"
)

func Run() {
	root := &cobra.Command{
		Use:   "machina",
		Short: "machina machine controller",
	}

	root.AddCommand(controller.NewCommand())
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Println(version.String())
		},
	})

	if err := root.Execute(); err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}
}
