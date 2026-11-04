package main

import (
	"fmt"
	"math/rand/v2"
)

// ownerCode is the fictional owner of every container unloaded in the harbour:
// CKE, for Clever Kubernetes Engine. "U" is the ISO 6346 category for freight containers.
const ownerCode = "CKEU"

// letterValues follows ISO 6346: A=10, then +1 per letter, skipping multiples of 11.
var letterValues = func() map[rune]int {
	m := map[rune]int{}
	v := 10
	for c := 'A'; c <= 'Z'; c++ {
		if v%11 == 0 {
			v++
		}
		m[c] = v
		v++
	}
	return m
}()

// checkDigit returns the ISO 6346 check digit of a 10-character code (owner, category, serial).
func checkDigit(code string) int {
	sum := 0
	for i, c := range code {
		n, ok := letterValues[c]
		if !ok {
			n = int(c - '0')
		}
		sum += n << i
	}
	return sum % 11 % 10
}

// newContainerCode returns a valid container number, for example CKEU 482915 3.
func newContainerCode() string {
	base := fmt.Sprintf("%s%06d", ownerCode, rand.IntN(1_000_000))
	return fmt.Sprintf("%s%d", base, checkDigit(base))
}
