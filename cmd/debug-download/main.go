package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/cloudraid/cloudraid/internal/alist"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: debug-download <alist-path>")
		os.Exit(2)
	}
	client := alist.New("http://127.0.0.1:5244", "admin", "test123456")
	if err := client.Login(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	rc, err := client.Download(context.Background(), os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(len(b))
}
