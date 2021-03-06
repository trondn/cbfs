package main

import (
	"bytes"
	"io"
	"math/rand"
	"reflect"
	"testing"
)

const expSize = 64 * 1024

func TestMultiReader(t *testing.T) {
	t.Parallel()

	randomSrc := randomDataMaker{rand.NewSource(1028890720402726901)}
	lr := io.LimitReader(&randomSrc, expSize)

	r1, r2 := newMultiReader(lr)

	b1 := &bytes.Buffer{}
	b2 := &bytes.Buffer{}

	rs := make(chan copyRes, 2)

	go bgCopy(b1, r1, rs)
	go bgCopy(b2, r2, rs)

	res1 := <-rs
	res2 := <-rs

	if res1.e != nil || res2.e != nil {
		t.Logf("Error copying data: %v/%v", res1.e, res2.e)
	}

	if res1.s != res2.s || res1.s != expSize {
		t.Fatalf("Read %v/%v bytes, expected %v",
			res1.s, res2.s, expSize)
	}

	if !reflect.DeepEqual(b1, b2) {
		t.Fatalf("Didn't read the same data from the two things")
	}
}
