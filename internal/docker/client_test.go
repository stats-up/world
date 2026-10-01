package docker

import "testing"

func TestDemux(t *testing.T) {
	frame := func(stream byte, s string) []byte {
		h := []byte{stream, 0, 0, 0, 0, 0, 0, byte(len(s))}
		return append(h, s...)
	}
	data := append(frame(1, "hola\n"), frame(2, "error\n")...)
	if got := demux(data); got != "hola\nerror\n" {
		t.Fatalf("demux = %q", got)
	}
	if got := demux([]byte("sin cabecera")); got != "sin cabecera" {
		t.Fatalf("demux sin cabecera = %q", got)
	}
}
