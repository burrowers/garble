# Benchmark

Date: {{ .Date }}

Commit: {{ .CommitHash }}

GOOS: {{ .GOOS }}

## Binary size


|   |{{range .Headers}} {{ . }} |{{end}}
| ------------- |{{range .Headers}} ------------- |{{end}}{{range .BinarySizeRows}} 
| {{range .}} {{.}} |{{end}}{{end}}
