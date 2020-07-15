package literals

import (
	"encoding/binary"
	"go/ast"
	"go/token"
)

func getBoundsCheck(pos string) *ast.AssignStmt {
	return &ast.AssignStmt{
		Lhs: []ast.Expr{&ast.Ident{Name: "_"}},
		Tok: token.ASSIGN,
		Rhs: []ast.Expr{
			&ast.IndexExpr{
				X: &ast.Ident{Name: "data"},
				Index: &ast.BasicLit{
					Kind:  token.INT,
					Value: pos,
				},
			},
		},
	}
}

func obfuscateUint8(data uint8) *ast.CallExpr {
	obfuscator := getObfuscator()
	block := obfuscator.Obfuscate([]byte{byte(data)})
	block.List = append(block.List,
		&ast.ReturnStmt{
			Results: []ast.Expr{
				&ast.CallExpr{
					Fun: &ast.Ident{Name: "uint8"},
					Args: []ast.Expr{
						&ast.IndexExpr{
							X: &ast.Ident{Name: "data"},
							Index: &ast.BasicLit{
								Kind:  token.INT,
								Value: "0",
							},
						},
					},
				},
			},
		})

	return getCallexpr(&ast.Ident{Name: "uint8"}, block)
}

func obfuscateUint16(data uint16) *ast.CallExpr {
	obfuscator := getObfuscator()
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, data)
	block := obfuscator.Obfuscate(b)

	convertExpr := &ast.BinaryExpr{
		X: &ast.CallExpr{
			Fun: &ast.Ident{Name: "uint16"},
			Args: []ast.Expr{
				&ast.IndexExpr{
					X: &ast.Ident{Name: "data"},
					Index: &ast.BasicLit{
						Kind:  token.INT,
						Value: "0",
					},
				},
			},
		},
		Op: token.OR,
		Y: &ast.BinaryExpr{
			X: &ast.CallExpr{
				Fun: &ast.Ident{Name: "uint16"},
				Args: []ast.Expr{
					&ast.IndexExpr{
						X: &ast.Ident{Name: "data"},
						Index: &ast.BasicLit{
							Kind:  token.INT,
							Value: "1",
						},
					},
				},
			},
			Op: token.SHL,
			Y: &ast.BasicLit{
				Kind:  token.INT,
				Value: "8",
			},
		},
	}

	block.List = append(block.List, getBoundsCheck("1"), getReturnStmt(convertExpr))

	return getCallexpr(&ast.Ident{Name: "uint16"}, block)
}

func obfuscateUint32(data uint32) *ast.CallExpr {
	obfuscator := getObfuscator()
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, data)
	block := obfuscator.Obfuscate(b)

	convertExpr := &ast.BinaryExpr{
		X: &ast.BinaryExpr{
			X: &ast.BinaryExpr{
				X: &ast.CallExpr{
					Fun: &ast.Ident{Name: "uint32"},
					Args: []ast.Expr{
						&ast.IndexExpr{
							X: &ast.Ident{Name: "data"},
							Index: &ast.BasicLit{
								Kind:  token.INT,
								Value: "0",
							},
						},
					},
				},
				Op: token.OR,
				Y: &ast.BinaryExpr{
					X: &ast.CallExpr{
						Fun: &ast.Ident{Name: "uint32"},
						Args: []ast.Expr{
							&ast.IndexExpr{
								X: &ast.Ident{Name: "data"},
								Index: &ast.BasicLit{
									Kind:  token.INT,
									Value: "1",
								},
							},
						},
					},
					Op: token.SHL,
					Y: &ast.BasicLit{
						Kind:  token.INT,
						Value: "8",
					},
				},
			},
			Op: token.OR,
			Y: &ast.BinaryExpr{
				X: &ast.CallExpr{
					Fun: &ast.Ident{Name: "uint32"},
					Args: []ast.Expr{
						&ast.IndexExpr{
							X: &ast.Ident{Name: "data"},
							Index: &ast.BasicLit{
								Kind:  token.INT,
								Value: "2",
							},
						},
					},
				},
				Op: token.SHL,
				Y: &ast.BasicLit{
					Kind:  token.INT,
					Value: "16",
				},
			},
		},
		Op: token.OR,
		Y: &ast.BinaryExpr{
			X: &ast.CallExpr{
				Fun: &ast.Ident{Name: "uint32"},
				Args: []ast.Expr{
					&ast.IndexExpr{
						X: &ast.Ident{Name: "data"},
						Index: &ast.BasicLit{
							Kind:  token.INT,
							Value: "3",
						},
					},
				},
			},
			Op: token.SHL,
			Y: &ast.BasicLit{
				Kind:  token.INT,
				Value: "24",
			},
		},
	}

	block.List = append(block.List, getBoundsCheck("3"), getReturnStmt(convertExpr))

	return getCallexpr(&ast.Ident{Name: "uint32"}, block)
}

func obfuscateUint64(data uint64) *ast.CallExpr {
	obfuscator := getObfuscator()
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, data)
	block := obfuscator.Obfuscate(b)
	convertExpr := &ast.BinaryExpr{
		X: &ast.BinaryExpr{
			X: &ast.BinaryExpr{
				X: &ast.BinaryExpr{
					X: &ast.BinaryExpr{
						X: &ast.BinaryExpr{
							X: &ast.BinaryExpr{
								X: &ast.CallExpr{
									Fun: &ast.Ident{Name: "uint64"},
									Args: []ast.Expr{
										&ast.IndexExpr{
											X: &ast.Ident{Name: "data"},
											Index: &ast.BasicLit{
												Kind:  token.INT,
												Value: "0",
											},
										},
									},
								},
								Op: token.OR,
								Y: &ast.BinaryExpr{
									X: &ast.CallExpr{
										Fun: &ast.Ident{Name: "uint64"},
										Args: []ast.Expr{
											&ast.IndexExpr{
												X: &ast.Ident{Name: "data"},
												Index: &ast.BasicLit{
													Kind:  token.INT,
													Value: "1",
												},
											},
										},
									},
									Op: token.SHL,
									Y: &ast.BasicLit{
										Kind:  token.INT,
										Value: "8",
									},
								},
							},
							Op: token.OR,
							Y: &ast.BinaryExpr{
								X: &ast.CallExpr{
									Fun: &ast.Ident{Name: "uint64"},
									Args: []ast.Expr{
										&ast.IndexExpr{
											X: &ast.Ident{Name: "data"},
											Index: &ast.BasicLit{
												Kind:  token.INT,
												Value: "2",
											},
										},
									},
								},
								Op: token.SHL,
								Y: &ast.BasicLit{
									Kind:  token.INT,
									Value: "16",
								},
							},
						},
						Op: token.OR,
						Y: &ast.BinaryExpr{
							X: &ast.CallExpr{
								Fun: &ast.Ident{Name: "uint64"},
								Args: []ast.Expr{
									&ast.IndexExpr{
										X: &ast.Ident{Name: "data"},
										Index: &ast.BasicLit{
											Kind:  token.INT,
											Value: "3",
										},
									},
								},
							},
							Op: token.SHL,
							Y: &ast.BasicLit{
								Kind:  token.INT,
								Value: "24",
							},
						},
					},
					Op: token.OR,
					Y: &ast.BinaryExpr{
						X: &ast.CallExpr{
							Fun: &ast.Ident{Name: "uint64"},
							Args: []ast.Expr{
								&ast.IndexExpr{
									X: &ast.Ident{Name: "data"},
									Index: &ast.BasicLit{
										Kind:  token.INT,
										Value: "4",
									},
								},
							},
						},
						Op: token.SHL,
						Y: &ast.BasicLit{
							Kind:  token.INT,
							Value: "32",
						},
					},
				},
				Op: token.OR,
				Y: &ast.BinaryExpr{
					X: &ast.CallExpr{
						Fun: &ast.Ident{Name: "uint64"},
						Args: []ast.Expr{
							&ast.IndexExpr{
								X: &ast.Ident{Name: "data"},
								Index: &ast.BasicLit{
									Kind:  token.INT,
									Value: "5",
								},
							},
						},
					},
					Op: token.SHL,
					Y: &ast.BasicLit{
						Kind:  token.INT,
						Value: "40",
					},
				},
			},
			Op: token.OR,
			Y: &ast.BinaryExpr{
				X: &ast.CallExpr{
					Fun: &ast.Ident{Name: "uint64"},
					Args: []ast.Expr{
						&ast.IndexExpr{
							X: &ast.Ident{Name: "data"},
							Index: &ast.BasicLit{
								Kind:  token.INT,
								Value: "6",
							},
						},
					},
				},
				Op: token.SHL,
				Y: &ast.BasicLit{
					Kind:  token.INT,
					Value: "48",
				},
			},
		},
		Op: token.OR,
		Y: &ast.BinaryExpr{
			X: &ast.CallExpr{
				Fun: &ast.Ident{Name: "uint64"},
				Args: []ast.Expr{
					&ast.IndexExpr{
						X: &ast.Ident{Name: "data"},
						Index: &ast.BasicLit{
							Kind:  token.INT,
							Value: "7",
						},
					},
				},
			},
			Op: token.SHL,
			Y: &ast.BasicLit{
				Kind:  token.INT,
				Value: "56",
			},
		},
	}

	block.List = append(block.List, getBoundsCheck("7"), getReturnStmt(convertExpr))

	return getCallexpr(&ast.Ident{Name: "uint64"}, block)
}

func obfuscateUint(data uint) *ast.CallExpr {
	return &ast.CallExpr{
		Fun: &ast.Ident{
			Name: "uint",
		},
		Args: []ast.Expr{
			obfuscateUint64(uint64(data)),
		},
	}
}

func obfuscateUintptr(data uintptr) *ast.CallExpr {
	return &ast.CallExpr{
		Fun: &ast.Ident{
			Name: "uintptr",
		},
		Args: []ast.Expr{
			obfuscateUint64(uint64(data)),
		},
	}
}

func obfuscateInt8(data int8) *ast.CallExpr {
	return &ast.CallExpr{
		Fun: &ast.Ident{
			Name: "int8",
		},
		Args: []ast.Expr{
			obfuscateUint8(uint8(data)),
		},
	}
}

func obfuscateInt16(data int16) *ast.CallExpr {
	return &ast.CallExpr{
		Fun: &ast.Ident{
			Name: "int16",
		},
		Args: []ast.Expr{
			obfuscateUint16(uint16(data)),
		},
	}
}

func obfuscateInt32(data int32) *ast.CallExpr {
	return &ast.CallExpr{
		Fun: &ast.Ident{
			Name: "int32",
		},
		Args: []ast.Expr{
			obfuscateUint32(uint32(data)),
		},
	}
}

func obfuscateInt64(data int64) *ast.CallExpr {
	return &ast.CallExpr{
		Fun: &ast.Ident{
			Name: "int64",
		},
		Args: []ast.Expr{
			obfuscateUint64(uint64(data)),
		},
	}
}

func obfuscateInt(data int) *ast.CallExpr {
	return &ast.CallExpr{
		Fun: &ast.Ident{
			Name: "int",
		},
		Args: []ast.Expr{
			obfuscateUint64(uint64(data)),
		},
	}
}

func obfuscateFloat32(data float32) *ast.CallExpr {
	return &ast.CallExpr{
		Fun: &ast.Ident{
			Name: "float32",
		},
		Args: []ast.Expr{
			obfuscateUint32(uint32(data)),
		},
	}
}

func obfuscateFloat64(data float64) *ast.CallExpr {
	return &ast.CallExpr{
		Fun: &ast.Ident{
			Name: "float64",
		},
		Args: []ast.Expr{
			obfuscateUint64(uint64(data)),
		},
	}
}
