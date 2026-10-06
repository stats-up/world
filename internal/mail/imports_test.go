package mail

import (
	"strings"
	"testing"
)

const sampleLog = `Host1 Nb messages:                   340 messages
msg INBOX/1 {2048}  copied to INBOX/1  2.0 msgs/s  4.0 KiB/s 2.0 KiB copied ETA: Tue Oct  6 12:00:00 2026  170 s  339/340 msgs left
msg INBOX/2 {4096}  copied to INBOX/2  2.0 msgs/s  4.0 KiB/s 6.0 KiB copied ETA: Tue Oct  6 12:00:00 2026  169 s  300/340 msgs left
`

const sampleSummary = `++++ Statistics
Messages transferred                    : 38 
Messages skipped                        : 302
Total bytes transferred                 : 1234567 (1.177 MiB)
Detected 2 errors
`

func TestParseImportLog(t *testing.T) {
	if s := ParseImportLog("Host1 Nb messages:          340 messages\n"); s.Total != 340 || s.Processed != 0 {
		t.Fatalf("al contar: %+v", s)
	}
	s := ParseImportLog(sampleLog)
	if s.Total != 340 || s.Processed != 40 || s.Done || s.Transferred != 2 || s.Bytes != 6<<10 || s.ETA != 169 {
		t.Fatalf("en curso: %+v", s)
	}
	s = ParseImportLog(sampleLog + sampleSummary)
	if !s.Done || s.Transferred != 38 || s.Bytes != 1234567 || s.Errors != 2 || s.Processed != 340 {
		t.Fatalf("final: %+v", s)
	}
	if s := ParseImportLog(""); s != (Summary{}) {
		t.Fatalf("vacío: %+v", s)
	}
}

func TestImportArgs(t *testing.T) {
	args := strings.Join(importArgs(ImportRequest{Email: "fernando.dc@statsup.cl", Host: "38.18.230.11", Port: 993, User: "fernando.dc@statsup.cl", Password: "nunca"}, 5), " ")
	if !strings.HasPrefix(args, "/usr/bin/imapsync ") {
		t.Error("falta el binario")
	}
	for _, want := range []string{"--host1 38.18.230.11", "--ssl1", "--host2 world-stalwart", "--user2 fernando.dc@statsup.cl%admin", "--logfile import-5.log", "--automap", "--log --logdir"} {
		if !strings.Contains(args, want) {
			t.Errorf("falta %q en %s", want, args)
		}
	}
	if strings.Contains(args, "nunca") {
		t.Error("la contraseña no va en la línea de comandos")
	}
	if args := importArgs(ImportRequest{Host: "h", Port: 143, User: "u"}, 1); !strings.Contains(strings.Join(args, " "), "--tls1") {
		t.Error("puerto 143 usa STARTTLS")
	}
}

func TestExitMessage(t *testing.T) {
	if !strings.Contains(exitMessage(161, 0), "contraseña de origen") {
		t.Error("161 = credencial de origen")
	}
	if !strings.Contains(exitMessage(111, 3), "3 errores") {
		t.Error("111 informa los errores")
	}
}

func TestLogTrackerIncremental(t *testing.T) {
	var tr logTracker
	// El log llega en trozos y a veces con una línea a medio escribir.
	cut := strings.Index(sampleLog, "copied to INBOX/2")
	tr.feed(sampleLog[:cut])
	if tr.sum.Transferred != 1 || tr.sum.Processed != 1 {
		t.Fatalf("primer trozo: %+v", tr.sum)
	}
	tr.feed(sampleLog[cut:])
	tr.feed(sampleSummary)
	if s := tr.sum; s.Transferred != 38 || !s.Done || s.Bytes != 1234567 || s.ETA != 0 {
		t.Fatalf("final: %+v", s)
	}
}
