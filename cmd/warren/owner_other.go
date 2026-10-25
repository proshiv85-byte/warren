//go:build !unix

package main

import "os"

func copyOwner(os.FileInfo, string) {}
