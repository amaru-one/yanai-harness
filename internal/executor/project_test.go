package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yanai/yanai-harness/internal/config"
	"github.com/yanai/yanai-harness/internal/workflow"
)

// This standard-library-only fixture exercises a real Go toolchain and, when
// requested, real PostgreSQL using its wire protocol against a disposable trust
// authenticated local instance. It touches only a connection-local temp table.
const postgresFixture = `package sample
import("encoding/binary";"io";"net";"net/url";"os";"testing";"time")
func TestDatabase(t *testing.T){
 u,e:=url.Parse(os.Getenv("YANAI_TEST_ADMIN_URL"));if e!=nil||u.Host==""{t.Fatal("required database URL missing")}
 c,e:=net.DialTimeout("tcp",u.Host,3*time.Second);if e!=nil{t.Fatal(e)};defer c.Close();c.SetDeadline(time.Now().Add(5*time.Second))
 body:=[]byte("user\x00"+u.User.Username()+"\x00database\x00"+u.Path[1:]+"\x00\x00")
 packet:=make([]byte,8+len(body));binary.BigEndian.PutUint32(packet,uint32(len(packet)));binary.BigEndian.PutUint32(packet[4:],196608);copy(packet[8:],body)
 if _,e=c.Write(packet);e!=nil{t.Fatal(e)}
 read:=func()(byte,[]byte){h:=make([]byte,5);if _,e:=io.ReadFull(c,h);e!=nil{t.Fatal(e)};n:=int(binary.BigEndian.Uint32(h[1:]))-4;if n<0||n>1<<20{t.Fatal("invalid server frame")};b:=make([]byte,n);if _,e:=io.ReadFull(c,b);e!=nil{t.Fatal(e)};if h[0]=='E'{t.Fatalf("database error: %q",b)};return h[0],b}
 for {kind,b:=read();if kind=='R'&&binary.BigEndian.Uint32(b)!=0{t.Fatal("fixture needs disposable trust-auth PostgreSQL")};if kind=='Z'{break}}
 query:=[]byte("CREATE TEMP TABLE fixture(value integer); INSERT INTO fixture VALUES (19),(23); SELECT SUM(value)::text FROM fixture;\x00")
 packet=make([]byte,5+len(query));packet[0]='Q';binary.BigEndian.PutUint32(packet[1:],uint32(4+len(query)));copy(packet[5:],query);if _,e=c.Write(packet);e!=nil{t.Fatal(e)}
 found:=false
 for {kind,b:=read();if kind=='D'{if len(b)!=8||binary.BigEndian.Uint16(b)!=1||string(b[6:])!="42"{t.Fatalf("unexpected database result %q",b)};found=true};if kind=='Z'{break}}
 if !found{t.Fatal("database assertion never executed")}
}
`

func TestNativeGenericProjectsWithRealChecks(t *testing.T) {
	for _, kind := range []string{"root", "nested", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			admin := ""
			if kind == "postgres" {
				admin = os.Getenv("YANAI_TEST_ADMIN_URL")
				if admin == "" {
					t.Skip("set YANAI_TEST_ADMIN_URL to a disposable local trust-auth PostgreSQL to run real database assertions")
				}
			}
			repo := t.TempDir()
			workspace := t.TempDir()
			module := "."
			if kind == "nested" {
				module = "services/api"
			}
			put(t, repo, filepath.Join(module, "go.mod"), "module example.test/independent\n\ngo 1.26.6\n")
			put(t, repo, filepath.Join(module, "value.go"), "package sample\nfunc Value() int { return 42 }\n")
			put(t, repo, filepath.Join(module, "value_test.go"), "package sample\nimport \"testing\"\nfunc TestValue(t *testing.T) { if Value()!=42 { t.Fatal(\"wrong value\") } }\n")
			if kind == "postgres" {
				put(t, repo, "database_test.go", postgresFixture)
			}
			git(t, repo, "init", "-q")
			git(t, repo, "add", ".")
			git(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "fixture")
			store, err := workflow.OpenStore(filepath.Join(workspace, "workflow.db"), "generic-checks")
			must(t, err)
			defer store.Close()
			env := func(key string) string {
				raw, err := exec.Command("go", "env", key).Output()
				must(t, err)
				return strings.TrimSpace(string(raw))
			}
			inputs := map[string]string{}
			if admin != "" {
				inputs["YANAI_TEST_ADMIN_URL"] = admin
			}
			o := Options{Repo: config.Repo{Path: repo, ModuleDir: module, AllowedPaths: []string{"."}}, Store: store, Artifacts: workflow.ArtifactStore{Root: workspace}, Cycle: 1, GoCache: env("GOCACHE"), ModuleCache: env("GOMODCACHE"), CheckInputs: inputs}
			checks := []workflow.Check{
				{ID: "test", Args: []string{"go", "test", "-v", "-race", "-shuffle=on", "-count=1", "./..."}, Dir: module, TimeoutSeconds: 120},
				{ID: "vet", Args: []string{"go", "vet", "./..."}, Dir: module, TimeoutSeconds: 120},
				{ID: "build", Args: []string{"go", "build", "./..."}, Dir: module, TimeoutSeconds: 120},
			}
			if kind == "postgres" {
				checks[0].RequiredEnv = []string{"YANAI_TEST_ADMIN_URL"}
				checks[0].PostgresURLVar = "YANAI_TEST_ADMIN_URL"
			}
			approve(t, &o, checks, nil)
			native, err := Open(o)
			must(t, err)
			defer native.Close()
			for _, check := range checks {
				result, err := native.Check(context.Background(), check.ID)
				if err != nil {
					t.Fatalf("%s: %v\n%s", check.ID, err, result.Output)
				}
				if result.ExitCode != 0 || result.Before != result.After || result.Evidence.SHA256 == "" {
					t.Fatalf("invalid real evidence: %+v", result)
				}
				if check.PostgresURLVar != "" && (!result.DatabaseEnabled || !strings.Contains(result.Output, "--- PASS: TestDatabase")) {
					t.Fatal("database test did not execute", result.Output)
				}
			}
		})
	}
}
