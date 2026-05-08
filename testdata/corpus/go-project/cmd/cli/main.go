package main

import (
	"fmt"

	"example.com/demo/internal/auth"
)

// main is the entry point.
func main() {
	if err := run(); err != nil {
		fmt.Println(err)
	}
}

// run wires the components together.
func run() error {
	a, err := auth.Open("token")
	if err != nil {
		return err
	}
	Greet(a.User())
	return nil
}

// Greet prints a salutation.
func Greet(name string) {
	fmt.Println("hello,", name)
}
