package main

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"text/template"
	"time"
)

type report struct {
	Date           string
	GOOS           string
	CommitHash     string
	Headers        []string
	BinarySizeRows [][]string
}

type project struct {
	name          string
	path          string
	targetPackage string
}

type command struct {
	bin  string
	args []string
}

var projects = []*project{
	{"staticcheck", "go-tools", "./cmd/staticcheck"},
}

var binarySizeCommands = []command{
	{"go", []string{"build"}},
	{"go", []string{"build", "-trimpath", "-ldflags", "-s -w"}},
	{"garble", []string{"build"}},
	{"garble", []string{"-tiny", "build"}},
	{"garble", []string{"-literals", "build"}},
	{"garble", []string{"-tiny", "-literals", "build"}},
}

const (
	dateFormat = "2006-01-02"
	seedParam  = "-seed=ESIzRFVmd4g"
)

func exitAndSkipWorkFlow() {
	const skipWorkflowCode = 78
	os.Exit(skipWorkflowCode)
}

func alreadyProcessed(commitHash, outputFile string) bool {
	file, err := os.Open(outputFile)
	if err != nil {
		return false
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), commitHash) {
			return true
		}
	}
	return false
}

func fileSize(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

//TODO: Add comma formating
func formatSize(size int64) string {
	return fmt.Sprintf("%d", size)
}

func diffSize(firstSize, secondSize int64) string {
	if firstSize == secondSize {
		return formatSize(firstSize)
	}

	diff := float64(secondSize-firstSize) / float64(firstSize) * 100
	return fmt.Sprintf("%s (%.2f%%)", formatSize(secondSize), diff)
}

func processProject(p *project) ([]string, error) {
	row := []string{p.name}

	var tempFiles []string
	defer func() {
		for _, file := range tempFiles {
			os.Remove(file)
		}
	}()

	var firstSize int64
	for i, command := range binarySizeCommands {
		tempOutputFile, err := ioutil.TempFile("", p.name)
		if err != nil {
			return nil, err
		}
		tempOutputFile.Close()
		tempFiles = append(tempFiles, tempOutputFile.Name())

		buildCommand := []string{command.bin}
		if runtime.GOOS == "windows" {
			buildCommand[0] += ".exe"
		}
		if command.bin == "garble" {
			buildCommand = append(buildCommand, seedParam)
		}
		buildCommand = append(buildCommand, command.args...)
		buildCommand = append(buildCommand, "-o", tempOutputFile.Name(), p.targetPackage)

		cmd := exec.Command(buildCommand[0], buildCommand[1:]...)
		cmd.Dir = p.path

		if output, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("build error: %v\n\n%s", err, string(output))
		}

		size, err := fileSize(tempOutputFile.Name())
		if err != nil {
			return nil, err
		}

		if i == 0 {
			firstSize = size
			row = append(row, formatSize(size))
			continue
		}

		row = append(row, diffSize(firstSize, size))
	}

	return row, nil
}

func main() {
	if len(os.Args) != 4 {
		panic("invalid usage: go run benchmark.go <commit> <input> <output>")
	}

	commitHash, inputPath, outputPath := os.Args[1], os.Args[2], os.Args[3]

	if alreadyProcessed(commitHash, outputPath) {
		exitAndSkipWorkFlow()
	}

	r := &report{
		Date:       time.Now().Format(dateFormat),
		GOOS:       runtime.GOOS,
		CommitHash: commitHash,
	}

	for _, command := range binarySizeCommands {
		header := command.bin + " " + strings.Join(command.args, " ")
		r.Headers = append(r.Headers, header)
	}

	for _, p := range projects {
		row, err := processProject(p)
		if err != nil {
			panic(err)
		}
		r.BinarySizeRows = append(r.BinarySizeRows, row)
	}

	tmpl, err := template.ParseFiles(inputPath)
	if err != nil {
		panic(err)
	}

	outputFile, err := os.Create(outputPath)
	if err != nil {
		panic(err)
	}
	defer outputFile.Close()

	if err := tmpl.Execute(outputFile, r); err != nil {
		panic(err)
	}
}
