package main

import "fmt"

var globalVar = "global value"

func globalFunc() { fmt.Println("global func body") }

func main() {
	fmt.Println(globalVar)
	globalFunc()
}
