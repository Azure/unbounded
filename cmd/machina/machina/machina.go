package machina

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/project-unbounded/unbounded-kube/cmd/machina/machina/controller"
)

func Run() {
	root := &cobra.Command{
		Use:   "machina",
		Short: "machina machine controller",
	}

	root.AddCommand(controller.NewCommand())

	if err := root.Execute(); err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}
}
