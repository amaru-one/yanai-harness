package workflow

import (
	"strings"
	"testing"
)

func executableTicket() Ticket {
	return Ticket{ID: "T-1", Outputs: []string{"yanai-server/a.go", "yanai-server/SPEC.md"}, AllowedPaths: []string{"yanai-server"}}
}

func TestDecodeCandidateAcceptsOnlyCompleteAnswers(t *testing.T) {
	ticket := executableTicket()
	good := `{"schema_version":"1","ticket":"T-1","result":"change","explanation":"por qué",
		"files":[{"path":"yanai-server/a.go","operation":"source","source":"package a\n"},
		         {"path":"yanai-server/SPEC.md","operation":"unchanged"}]}`
	c, err := DecodeCandidate(good, ticket)
	if err != nil {
		t.Fatal(err)
	}
	if c.Result != CandidateChange || len(c.Files) != 2 || *c.Files[0].Source != "package a\n" {
		t.Fatalf("decoded = %+v", c)
	}

	// An empty file is a real file, not an omission: a pointer is what keeps
	// "" and "missing" distinguishable.
	empty := `{"schema_version":"1","ticket":"T-1","result":"change","explanation":"vacío",
		"files":[{"path":"yanai-server/a.go","operation":"source","source":""},
		         {"path":"yanai-server/SPEC.md","operation":"unchanged"}]}`
	if c, err = DecodeCandidate(empty, ticket); err != nil || *c.Files[0].Source != "" {
		t.Fatalf("an empty file was not accepted: %v %+v", err, c)
	}

	noChange := `{"schema_version":"1","ticket":"T-1","result":"no_change","explanation":"ya está cubierto"}`
	if c, err = DecodeCandidate(noChange, ticket); err != nil || c.Result != CandidateNoChange {
		t.Fatalf("no_change: %v %+v", err, c)
	}
}

func TestDecodeCandidateRejectsEverythingTruncationLooksLike(t *testing.T) {
	cases := map[string]string{
		"omitted output": `{"schema_version":"1","ticket":"T-1","result":"change","explanation":"x",
			"files":[{"path":"yanai-server/a.go","operation":"source","source":"a"}]}`,
		"undeclared output": `{"schema_version":"1","ticket":"T-1","result":"change","explanation":"x",
			"files":[{"path":"yanai-server/b.go","operation":"source","source":"a"},
			         {"path":"yanai-server/a.go","operation":"source","source":"a"},
			         {"path":"yanai-server/SPEC.md","operation":"unchanged"}]}`,
		"duplicate path": `{"schema_version":"1","ticket":"T-1","result":"change","explanation":"x",
			"files":[{"path":"yanai-server/a.go","operation":"source","source":"a"},
			         {"path":"yanai-server/a.go","operation":"source","source":"b"},
			         {"path":"yanai-server/SPEC.md","operation":"unchanged"}]}`,
		"duplicate key":    `{"schema_version":"1","ticket":"T-1","result":"no_change","result":"change","explanation":"x"}`,
		"unknown field":    `{"schema_version":"1","ticket":"T-1","result":"no_change","explanation":"x","notes":"y"}`,
		"trailing content": `{"schema_version":"1","ticket":"T-1","result":"no_change","explanation":"x"} ¡listo!`,
		"truncated":        `{"schema_version":"1","ticket":"T-1","result":"no_change","explanation":"x"`,
		"wrong schema":     `{"schema_version":"2","ticket":"T-1","result":"no_change","explanation":"x"}`,
		"wrong ticket":     `{"schema_version":"1","ticket":"T-9","result":"no_change","explanation":"x"}`,
		"blank explanation": `{"schema_version":"1","ticket":"T-1","result":"change","explanation":"  ",
			"files":[{"path":"yanai-server/a.go","operation":"source","source":"a"},
			         {"path":"yanai-server/SPEC.md","operation":"unchanged"}]}`,
		"unknown operation": `{"schema_version":"1","ticket":"T-1","result":"change","explanation":"x",
			"files":[{"path":"yanai-server/a.go","operation":"append","source":"a"},
			         {"path":"yanai-server/SPEC.md","operation":"unchanged"}]}`,
		"source without content": `{"schema_version":"1","ticket":"T-1","result":"change","explanation":"x",
			"files":[{"path":"yanai-server/a.go","operation":"source"},
			         {"path":"yanai-server/SPEC.md","operation":"unchanged"}]}`,
		"unchanged carrying source": `{"schema_version":"1","ticket":"T-1","result":"change","explanation":"x",
			"files":[{"path":"yanai-server/a.go","operation":"source","source":"a"},
			         {"path":"yanai-server/SPEC.md","operation":"unchanged","source":"x"}]}`,
		"unsupported mode": `{"schema_version":"1","ticket":"T-1","result":"change","explanation":"x",
			"files":[{"path":"yanai-server/a.go","operation":"source","source":"a","mode":"777"},
			         {"path":"yanai-server/SPEC.md","operation":"unchanged"}]}`,
		"effective no-op": `{"schema_version":"1","ticket":"T-1","result":"change","explanation":"x",
			"files":[{"path":"yanai-server/a.go","operation":"unchanged"},
			         {"path":"yanai-server/SPEC.md","operation":"unchanged"}]}`,
		"no_change with files": `{"schema_version":"1","ticket":"T-1","result":"no_change","explanation":"x",
			"files":[{"path":"yanai-server/a.go","operation":"unchanged"}]}`,
		"unknown result":   `{"schema_version":"1","ticket":"T-1","result":"maybe","explanation":"x"}`,
		"fenced output":    "=== ARCHIVO: yanai-server/a.go ===\nx\n=== FIN ARCHIVO ===",
		"two objects":      `{"schema_version":"1","ticket":"T-1","result":"no_change","explanation":"x"}{"a":1}`,
		"invalid encoding": "{\"schema_version\":\"1\",\"ticket\":\"T-1\",\"result\":\"no_change\",\"explanation\":\"\xff\xfe\"}",
		"unencodable source": `{"schema_version":"1","ticket":"T-1","result":"change","explanation":"x",
			"files":[{"path":"yanai-server/a.go","operation":"source","source":"paquete \ud800 roto"},
			         {"path":"yanai-server/SPEC.md","operation":"unchanged"}]}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeCandidate(raw, executableTicket()); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestDeclaredOutputsRoundTripsTheChecklist(t *testing.T) {
	message := "TAREA_DE_EJECUCION\n\n" + TicketHeading + "T-7\n\nDescripción\n" +
		RenderDeclaredOutputs([]string{"yanai-server/a.go", "yanai-server/SPEC.md"}) +
		"\n# Cómo entregar\n\n" + "instrucciones"
	ticket, outputs := DeclaredOutputs(message)
	if ticket != "T-7" || len(outputs) != 2 || outputs[0] != "yanai-server/a.go" || outputs[1] != "yanai-server/SPEC.md" {
		t.Fatalf("ticket=%q outputs=%v", ticket, outputs)
	}
	if !strings.Contains(message, DeclaredOutputsHeading) {
		t.Fatal("the rendered request lost its checklist heading")
	}
}
