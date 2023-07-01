# Control Flow Obfuscation

> **This feature is experimental**. To enable it, set the environment variable `GARBLE_EXPERIMENTAL_CONTROLFLOW=1`

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
// Obfuscate with defaults parameters
//garble:controlflow
func main() {
	println("Hello world!")
}


// Obfuscate with maximum parameters
//garble:controlflow block_splits=max junk_jumps=max flatten_passes=max
func main() {
    println("Hello world!")
}
```

### Parameter explanation

> Unlike other garble features (which just work), we recommend that you understand how parameters affect control flow obfuscation and which caveats exist.

> Code snippets below without name obfuscation, for better readability.

#### Block splitting

Parameter: `block_splits` (default: `0`)

> Warning: this param affects resulting binary only when used in combination with [flattening](#control-flow-flattening)


Block splitting splits the largest SSA block into 2 parts of random size, this is done `block_splits` times at most. If there is no more suitable block to split (number of ssa instructions in block is less than 3), splitting stops.

This param is very useful if your code has few branches (`if`, `switch` etc.).

Input:
```go
package main

// Note that the block_splits value is "max", so code blocks are split into the smallest possible blocks.
//garble:controlflow flatten_passes=0 junk_jumps=0 block_splits=max
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

Parameter: `junk_jumps` (default: `0`, maximum: `1024`)

> Warning: this param affects resulting binary only when used in combination with [flattening](#control-flow-flattening)

Junk jumps adds jumps to random blocks `junk_jumps` times, these inserted jumps can also form a chain. This param is useful for linearly increasing the functions complexity.

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

Parameter: `flatten_passes` (default: `1`, maximum: `4`)


This parameter completely [flattens the control flow](https://github.com/obfuscator-llvm/obfuscator/wiki/Control-Flow-Flattening) `flatten_passes` times, which makes analysing the logic of the function very difficult
This is the most important parameter without which the other parameters have no effect on the resulting binary.

> Warning: Unlike junk jumps, this parameter increases control flow complexity non-linearly. In most cases we do not recommend specifying a value greater than 3. Check [Benchmark](#complexity-benchmark)


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

### Caveats

* Obfuscation breaks the lazy iteration over maps. See: [ssa2ast/polyfill.go](../internal/ssa2ast/polyfill.go)
* Generic functions not supported

### Complexity benchmark

We approximate complexity by counting the number of blocks.

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

Before obfuscation this function has `8` blocks.

| flatten_passes | block_splits | junk_jumps | block_count |
|----------------|--------------|------------|-------------|
| 1              | 0            | 0          | 32          |
| 1              | 10           | 0          | 62          |
| 1              | 100          | 0          | 95          |
| 1              | 1024         | 0          | 95          |
| 1              | 0            | 10         | 62          |
| 1              | 10           | 10         | 92          |
| 1              | 100          | 10         | 125         |
| 1              | 1024         | 10         | 125         |
| 1              | 0            | 100        | 332         |
| 1              | 10           | 100        | 362         |
| 1              | 100          | 100        | 395         |
| 1              | 1024         | 100        | 395         |
| 2              | 0            | 0          | 123         |
| 2              | 10           | 0          | 233         |
| 2              | 100          | 0          | 354         |
| 2              | 1024         | 0          | 354         |
| 2              | 0            | 10         | 233         |
| 2              | 10           | 10         | 343         |
| 2              | 100          | 10         | 464         |
| 2              | 1024         | 10         | 464         |
| 2              | 0            | 100        | 1223        |
| 2              | 10           | 100        | 1333        |
| 2              | 100          | 100        | 1454        |
| 2              | 1024         | 100        | 1454        |
| 3              | 0            | 0          | 486         |
| 3              | 10           | 0          | 916         |
| 3              | 100          | 0          | 1389        |
| 3              | 1024         | 0          | 1389        |
| 3              | 0            | 10         | 916         |
| 3              | 10           | 10         | 1346        |
| 3              | 100          | 10         | 1819        |
| 3              | 1024         | 10         | 1819        |
| 3              | 0            | 100        | 4786        |
| 3              | 10           | 100        | 5216        |
| 3              | 100          | 100        | 5689        |
| 3              | 1024         | 100        | 5689        |
| 4              | 0            | 0          | 1937        |
| 4              | 10           | 0          | 3647        |
| 4              | 100          | 0          | 5528        |
| 4              | 1024         | 0          | 5528        |
| 4              | 0            | 10         | 3647        |
| 4              | 10           | 10         | 5357        |
| 4              | 100          | 10         | 7238        |
| 4              | 1024         | 10         | 7238        |
| 4              | 0            | 100        | 19037       |
| 4              | 10           | 100        | 20747       |
| 4              | 100          | 100        | 22628       |
| 4              | 1024         | 100        | 22628       |
