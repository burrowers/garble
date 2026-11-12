// Copyright (c) 2026 The Garble Authors.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
)

func main() {
	r := httptest.NewRecorder()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"message": "literal-obfuscation-http-probe"})
	})
	h.ServeHTTP(r, httptest.NewRequest("GET", "/", nil))
	println(r.Code, r.Header().Get("Content-Type"), r.Body.String())
}
