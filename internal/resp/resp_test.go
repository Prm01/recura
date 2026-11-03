package resp

import (
	"bufio"
	"strings"
	"testing"
)

func TestReadReplies(t *testing.T) {
	in := "+OK\r\n:42\r\n$5\r\nhello\r\n$-1\r\n*2\r\n$1\r\na\r\n:7\r\n-ERR bad\r\n"
	r := bufio.NewReader(strings.NewReader(in))
	want := []func(Value) bool{
		func(v Value) bool { return v.Kind == '+' && v.Str == "OK" },
		func(v Value) bool { return v.Int == 42 },
		func(v Value) bool { return v.Str == "hello" },
		func(v Value) bool { return v.Null },
		func(v Value) bool { return len(v.Array) == 2 && v.Array[0].Str == "a" && v.Array[1].Int == 7 },
		func(v Value) bool { return v.Kind == '-' && v.Str == "ERR bad" },
	}
	for i, ok := range want {
		v, err := read(r)
		if err != nil || !ok(v) {
			t.Fatalf("reply %d: got %+v, %v", i, v, err)
		}
	}
}
