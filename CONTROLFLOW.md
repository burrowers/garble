# Control Flow Obfuscation

> **Feature is experimental**. To activate it, set environment variable `GARBLE_EXPERIMENTAL_CONTROLFLOW=1`

### Mechanism


Control flow obfuscation works in several stages:

1) Collect functions with `//garble:controlflow` comment
2) Converts [go/ast](https://pkg.go.dev/go/ast) representation to [go/ssa](https://pkg.go.dev/golang.org/x/tools/go/ssa)
3) Applies [block splitting](#block-splitting)
4) Generates [junk jumps](#junk-jumps)
5) Applies [control flow flattening](#control-flow-flattening)
6) Converts go/ssa back into go/ast

### Example usage

```go
//garble:controlflow flatten_passes=1 junk_jumps=1 block_splits=1
func main() {
	println("Hellow world!")
}
```

### Parameter explanation

> Unlike other garble features (which just work), we recommend that you understand how parameters affect control flow obfuscation and what caveats exists.

> Code snippets below without name obfuscation for better readability.

#### Block splitting

Param: `block_splits` (default: `0`)

> Warning: this param affects resulting binary only when used in combination with [flattening](#control-flow-flattening)


Block splitting `block_splits` times splits largest block into 2 parts of random size. If there is no suitable block (number of ssa instructions in block is less than 3) work stops without errors.

This param is very useful if your code has few branches (`if`, `switch` etc.).

Input:
```go
package main

// Note that the block_splits value is very large, so code blocks are splitted into the smallest possible blocks.
//garble:controlflow flatten_passes=0 junk_jumps=0 block_splits=1024
func main() {
	println("1")
	println("2")
	println("3")
	println("4")
	println("5")
}
```

Result:

```go
func main() {
	{
		println("1")
		goto _s2a_l3
	}
_s2a_l1:
	{
		println("3")
		goto _s2a_l4
	}
_s2a_l2:
	{
		println("5")
		return
	}
_s2a_l3:
	{
		println("2")
		goto _s2a_l1
	}
_s2a_l4:
	{
		println("4")
		goto _s2a_l2
	}
}
 
```


#### Junk jumps

Param: `junk_jumps` (default: `0`)

> Warning: this param affects resulting binary only when used in combination with [flattening](#control-flow-flattening)

Junk jumps adds `junk_jumps` times to random blocks junk jump, can make a chain of jumps. This function is useful for linearly increasing function complexity.

Input:
```go
//garble:controlflow flatten_passes=0 junk_jumps=5 block_splits=0
func main() {
	if true {
		println("hello world")
	}
}
```

Result: 

```go
func main() {
	{
		if true {
			goto _s2a_l4
		} else {
			goto _s2a_l2
		}
	}
_s2a_l1:
	{
		println("hello world")
		goto _s2a_l7
	}
_s2a_l2:
	{
		return
	}
_s2a_l3:
	{
		goto _s2a_l2
	}
_s2a_l4:
	{
		goto _s2a_l6
	}
_s2a_l5:
	{
		goto _s2a_l3
	}
_s2a_l6:
	{
		goto _s2a_l1
	}
_s2a_l7:
	{
		goto _s2a_l5
	}
}
```

#### Control flow flattening

Param: `flatten_passes` (default: `1`)


This function completely flattens the control flow `flatten_passes` times, which makes analysing the logic of the function very difficult
This is the most important parameter without which the other parameters have no effect on the resulting binary.

> Warning: Unlike junk jumps, this parameter increases control flow complexity non-linearly. In most cases we do not recommend specifying a value greater than 3. Check [Benchmarks](#complexity-benchmark)


Input:
```go
//garble:controlflow flatten_passes=1 junk_jumps=0 block_splits=0
func main() {
	if true {
		println("hello world")
	} else {
		println("not hello world")
	}
}
```

Result:

```go
func main() {
	var _s2a_0 int
_s2a_l0:
	{
		goto _s2a_l2
	}
_s2a_l1:
	{
		println("not hello world")
		goto _s2a_l6
	}
_s2a_l2:
	{
		_s2a_1 := _s2a_0 == (int)(1)
		if _s2a_1 {
			goto _s2a_l9
		} else {
			goto _s2a_l8
		}
	}
_s2a_l3:
	{
		_s2a_2 := _s2a_0 == (int)(4)
		if _s2a_2 {
			goto _s2a_l4
		} else {
			goto _s2a_l5
		}
	}
_s2a_l4:
	{
		return
	}
_s2a_l5:
	{
		if true {
			goto _s2a_l7
		} else {
			goto _s2a_l11
		}
	}
_s2a_l6:
	{
		_s2a_0 = (int)(4)
		goto _s2a_l0
	}
_s2a_l7:
	{
		_s2a_0 = (int)(1)
		goto _s2a_l0
	}
_s2a_l8:
	{
		_s2a_3 := _s2a_0 == (int)(2)
		if _s2a_3 {
			goto _s2a_l1
		} else {
			goto _s2a_l12
		}
	}
_s2a_l9:
	{
		println("hello world")
		goto _s2a_l10
	}
_s2a_l10:
	{
		_s2a_0 = (int)(3)
		goto _s2a_l0
	}
_s2a_l11:
	{
		_s2a_0 = (int)(2)
		goto _s2a_l0
	}
_s2a_l12:
	{
		_s2a_4 := _s2a_0 == (int)(3)
		if _s2a_4 {
			goto _s2a_l4
		} else {
			goto _s2a_l3
		}
	}
}
```

### Complexity benchmark

By complexity, means number of blocks.

Analysed function:
```go
func sort(arr []int) []int {
	for i := 0; i < len(arr)-1; i++ {
		for j := 0; j < len(arr)-i-1; j++ {
			if arr[j] > arr[j+1] {
				arr[j], arr[j+1] = arr[j+1], arr[j]
			}
		}
	}
	return arr
}
```

With all parameter values equal to 0 number of blocks: `8`

| block_splits | blocks |
|--------------|--------|
| 0            | 8      |
| 1            | 9      |
| 2            | 10     |
| 10           | 18     |
| 100          | 21     |
| 1000         | 21     |


| junk_jumps | blocks |
|------------|--------|
| 0          | 8      |
| 1          | 9      |
| 2          | 10     |
| 10         | 18     |
| 100        | 108    |
| 1000       | 1008   |


| flatten_passes | blocks |
|----------------|--------|
| 0              | 8      |
| 1              | 32     |
| 2              | 123    |
| 3              | 486    |
| 4              | 1937   |
| 5              | 7740   |



### Caveats

* Obfuscation breaks the lazy iteration over maps. See: [ssa2ast/polyfill.go](internal/ssa2ast/polyfill.go)
* Generic functions not supported