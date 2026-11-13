//go:build windows

package main

import "unsafe"

func uintptrOf(p *uint16) uintptr { return uintptr(unsafe.Pointer(p)) }
