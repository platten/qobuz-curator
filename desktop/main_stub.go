//go:build !desktop

package main

import "fmt"

func main() {
	fmt.Println("Qobuz Curator Desktop must be built with Wails and the desktop build tag")
}
