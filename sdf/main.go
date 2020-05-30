package main

import "fmt"

const (
	cnst      = "Lorem"
	multiline = `First Line
Second Line`
)

var variable = "ipsum"

func main() {
	localVar := "dolor"

	reassign := "sit"
	reassign = "amet"

	fmt.Println(cnst)
	fmt.Println(multiline)
	fmt.Println(variable)
	fmt.Println(localVar)
	fmt.Println(reassign)

	fmt.Println("another literal")
}
