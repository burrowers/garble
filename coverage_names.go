// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// coverageCounterName retains the compiler's prefix grouping, but not the
// descriptive prefix emitted by cmd/cover.
func (tf *transformer) coverageCounterName(name string) string {
	prefix := tf.coverageNames.CounterPrefix
	if prefix != "" && strings.HasPrefix(name, prefix) {
		return hashWithPackage(tf.curPkg, prefix) + "_" + strings.TrimPrefix(name, prefix)
	}
	return ""
}

func (tf *transformer) transformCoverageConfig(flags []string) ([]string, error) {
	path := flagValue(flags, "-coveragecfg")
	if path == "" {
		return flags, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &tf.coverageNames); err != nil {
		return nil, fmt.Errorf("parse coverage configuration: %w", err)
	}
	if !tf.curPkg.toObfuscate() {
		return flags, nil
	}
	// Retain all other compiler settings, including fields added by newer Go
	// releases. Keep coverageNames in its original spelling for type checking
	// and staticLiteralInits.
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	for key, name := range map[string]string{
		"MetaVar":       tf.coverageNames.MetaVar,
		"PkgIdVar":      tf.coverageNames.PkgIdVar,
		"CounterPrefix": tf.coverageNames.CounterPrefix,
	} {
		if name == "" {
			continue
		}
		mapped := obfuscatedPackageObjectName(tf.curPkg, name)
		if key == "CounterPrefix" {
			mapped = tf.coverageCounterName(name)
		}
		cfg[key], err = json.Marshal(mapped)
		if err != nil {
			return nil, err
		}
	}
	file, err := os.CreateTemp(sharedTempDir, "coveragecfg-")
	if err != nil {
		return nil, err
	}
	if err := json.NewEncoder(file).Encode(cfg); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	return flagSetValue(flags, "-coveragecfg", file.Name()), nil
}
