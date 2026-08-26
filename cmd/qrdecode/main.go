// Command qrdecode decodes a QR Code PNG or SVG file and prints its payload.
// It exists for manual verification and for the deployment smoke test.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mvtp/qrurl/internal/qrdecode"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: qrdecode <file.png|file.svg>")
		os.Exit(2)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "qrdecode:", err)
		os.Exit(1)
	}
	var res *qrdecode.Result
	if strings.EqualFold(filepath.Ext(os.Args[1]), ".svg") {
		res, err = qrdecode.DecodeSVG(data)
	} else {
		res, err = qrdecode.DecodePNG(data)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "qrdecode:", err)
		os.Exit(1)
	}
	fmt.Printf("version=%d level=%s mask=%d corrections=%d\n", res.Version, res.Level, res.Mask, res.Corrections)
	fmt.Println(res.Text)
}
