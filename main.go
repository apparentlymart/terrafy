package main

import (
	"context"
	"fmt"
	"os"

	"github.com/apparentlymart/terrafy/internal/terrafy"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/terraform-exec/tfinstall"
	"golang.org/x/crypto/ssh/terminal"
)

func main() {
	isTerm := terminal.IsTerminal(int(os.Stderr.Fd()))
	width := 79
	if isTerm {
		w, _, err := terminal.GetSize(int(os.Stderr.Fd()))
		if err == nil {
			width = w
		}
	}

	// TODO: Make Terraform executable path customizable with a
	// command line option.
	execFile, err := tfinstall.Find(context.Background(), tfinstall.LookPath())
	if err != nil {
		fmt.Fprint(os.Stderr, "Error: Can't find 'terraform' executable in your PATH.\n\n")
		os.Exit(1)
	}

	opts := terrafy.Options{
		TerraformExec: execFile,
	}
	sourceFiles, diags := terrafy.Run(&opts)
	if len(diags) != 0 {
		wr := hcl.NewDiagnosticTextWriter(os.Stderr, sourceFiles, uint(width), isTerm)
		wr.WriteDiagnostics(diags)
	}
	if diags.HasErrors() {
		os.Exit(1)
	}
}
