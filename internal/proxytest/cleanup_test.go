package proxytest

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zystem/godepot/internal/meta"
)

type cleanReport struct {
	Enforced   bool `json:"enforced"`
	Considered int
	Kept       int
	Remove     []struct {
		Name, Version string
		Assets        int
		Bytes         int64
		Removed       bool
		Error         string
	}
	Bytes int64
	Freed int64
	Note  string
}

func (r *rig) report(method, repo string) cleanReport {
	r.t.Helper()
	a := r.expect(r.do(method, "/api/v1/repositories/"+repo+"/cleanup", nil), 200)
	var c cleanReport
	if err := json.Unmarshal(a.body, &c); err != nil {
		r.t.Fatalf("%v: %s", err, a.body)
	}
	return c
}

func (r *rig) versions(c cleanReport) string {
	var v []string
	for _, e := range c.Remove {
		v = append(v, e.Name+"@"+e.Version)
	}
	return strings.Join(v, ",")
}

// liveBytes is the sum of the sizes of the blobs that are there.
func (r *rig) liveBytes() int64 {
	r.t.Helper()
	rows, err := r.db.Query(context.Background(), meta.S(`SELECT COALESCE(SUM(size), 0) FROM blobs`))
	if err != nil {
		r.t.Fatal(err)
	}
	return meta.Int(rows[0][0])
}

// collect lets the grace time of the blob layer pass and runs the collector.
func (r *rig) collect() {
	r.t.Helper()
	r.advance(25 * time.Hour)
	for i := 0; i < 5; i++ {
		if _, err := r.layer.Collect(context.Background(), 1000); err != nil {
			r.t.Fatal(err)
		}
	}
}

const mvnClean = `{"name": "mvn", "format": "maven", "kind": "hosted", "cleanup": {"remove_older_days": 10, "keep_newest": 2%s}}`

func deployVersion(r *rig, v string, jar []byte) {
	r.deploy("mvn", "com/acme/app/"+v+"/app-"+v+".pom", mpom("com.acme", "app", v))
	r.deploy("mvn", "com/acme/app/"+v+"/app-"+v+".jar", jar)
}

// The report is what enforcing removes, and the bytes it says are freed are the bytes that are freed (docs/plan.md, step 7).
func TestCleanupReportEqualsEnforcement(t *testing.T) {
	r := newRig(t, repos(strings.Replace(mvnClean, "%s", "", 1)))
	r.trash.Days = 7
	shared := blobBytes(5000, 3)
	deployVersion(r, "1.0", blobBytes(4000, 1))
	deployVersion(r, "1.1", shared) // the same bytes as 2.0: kept by 2.0, so not freed with 1.1
	r.advance(20 * 24 * time.Hour)
	deployVersion(r, "1.2", blobBytes(3000, 4))
	deployVersion(r, "2.0", shared)
	r.advance(24 * time.Hour)
	deployVersion(r, "2.1", blobBytes(2000, 5))

	dry := r.report("GET", "mvn")
	if dry.Enforced || r.versions(dry) != "com/acme/app@1.0,com/acme/app@1.1" || dry.Kept != 2 || dry.Considered != 5 {
		t.Fatalf("the dry report: %+v (%s)", dry, r.versions(dry))
	}
	// 1.0 has a jar of 4000 and a pom; 1.1 has the shared jar and a pom. The shared jar is also in 2.0, so it is not freed.
	if dry.Freed >= dry.Bytes || dry.Freed < 4000 {
		t.Fatalf("freed %d of %d", dry.Freed, dry.Bytes)
	}
	before := r.liveBytes()
	// The dry report changed nothing; the policy has enforce off, so a run removes nothing either.
	if run := r.report("POST", "mvn"); run.Enforced || len(run.Remove) != 2 || run.Remove[0].Removed {
		t.Fatalf("a run with enforce off: %+v", run)
	}
	r.expect(r.get("/repository/mvn/com/acme/app/1.0/app-1.0.jar"), 200)

	// Turn enforce on (a new settings file), run, and see that it removed what the dry report named.
	set, err := parseSettings(repos(strings.Replace(mvnClean, "%s", `, "enforce": true`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	r.setSettings(set)
	run := r.report("POST", "mvn")
	if !run.Enforced || r.versions(run) != r.versions(dry) || run.Bytes != dry.Bytes || run.Freed != dry.Freed {
		t.Fatalf("enforcement %+v differs from the dry report %+v", run, dry)
	}
	for _, e := range run.Remove {
		if !e.Removed {
			t.Fatalf("not removed: %+v", e)
		}
	}
	r.expect(r.get("/repository/mvn/com/acme/app/1.0/app-1.0.jar"), 404)
	r.expect(r.get("/repository/mvn/com/acme/app/2.0/app-2.0.jar"), 200)
	md := string(r.expect(r.get("/repository/mvn/com/acme/app/maven-metadata.xml"), 200).body)
	if strings.Contains(md, "<version>1.0</version>") || !strings.Contains(md, "<version>2.1</version>") {
		t.Fatalf("metadata after the cleanup:\n%s", md)
	}
	// In the trash, nothing is freed yet.
	r.collect()
	if r.liveBytes() != before {
		t.Fatalf("the trash freed bytes: %d -> %d", before, r.liveBytes())
	}
	// A second run finds nothing more.
	if again := r.report("GET", "mvn"); len(again.Remove) != 0 {
		t.Fatalf("again: %+v", again)
	}
	// Emptied, the bytes the report said are freed.
	var items struct{ Items []struct{ ID string } }
	_ = json.Unmarshal(r.expect(r.get("/api/v1/trash?repository=mvn"), 200).body, &items)
	if len(items.Items) != 2 {
		t.Fatalf("the trash: %s", r.get("/api/v1/trash").body)
	}
	for _, it := range items.Items {
		r.expect(r.do("DELETE", "/api/v1/trash/"+it.ID, nil), 204)
	}
	r.collect()
	if freed := before - r.liveBytes(); freed != run.Freed {
		t.Fatalf("the report said %d freed, %d were", run.Freed, freed)
	}
	if r.auditCount("cleanup", "enforced=true") != 1 {
		t.Fatal("no audit line of the cleanup")
	}
}

func TestCleanupKeepRules(t *testing.T) {
	r := newRig(t, repos(`{"name": "mvn", "format": "maven", "kind": "hosted", "cleanup": {"remove_older_days": 0, "keep_read_days": 5, "keep": ["org/keep/*"], "keep_props": {"pin": "yes"}}}`))
	for _, v := range []string{"1.0", "1.1", "1.2", "1.3"} {
		deployVersion(r, v, blobBytes(1000, byte(v[2])))
	}
	r.deploy("mvn", "org/keep/lib/1.0/lib-1.0.jar", []byte("kept by its name"))
	r.expect(r.put("/repository/mvn/org/keep/lib/1.0/lib-1.0.pom", mpom("org.keep", "lib", "1.0")), 201)
	r.advance(30 * 24 * time.Hour)
	// 1.1 is read now (and the read is written), so it is held for 5 days; 1.2 was read 20 days ago.
	r.advance(-20 * 24 * time.Hour)
	r.expect(r.get("/repository/mvn/com/acme/app/1.2/app-1.2.jar"), 200)
	r.advance(20 * 24 * time.Hour)
	r.expect(r.get("/repository/mvn/com/acme/app/1.1/app-1.1.jar"), 200)
	if err := r.reads.Flush(context.Background(), r.db); err != nil {
		t.Fatal(err)
	}
	// 1.3 has a property on its jar.
	if _, err := r.db.Exec(context.Background(), meta.S(`UPDATE components SET props = '{"pin":"yes"}' WHERE repo = 'mvn' AND name = 'com/acme/app/1.3/app-1.3.jar'`)); err != nil {
		t.Fatal(err)
	}
	c := r.report("GET", "mvn")
	if got := r.versions(c); got != "com/acme/app@1.0,com/acme/app@1.2" && got != "com/acme/app@1.2,com/acme/app@1.0" {
		t.Fatalf("removed: %s (kept %d of %d)", got, c.Kept, c.Considered)
	}
	if c.Kept != 3 {
		t.Fatalf("kept %d", c.Kept)
	}
	// Nothing set: nothing removed, and the report says why.
	r2 := newRig(t, repos(`{"name": "mvn", "format": "maven", "kind": "hosted", "cleanup": {"keep_newest": 1}}`))
	deployVersion(r2, "1.0", []byte("x"))
	r2.advance(300 * 24 * time.Hour)
	if c := r2.report("GET", "mvn"); len(c.Remove) != 0 || !strings.Contains(c.Note, "remove_older_days") {
		t.Fatalf("no remove rule: %+v", c)
	}
}

func TestTrashRestore(t *testing.T) {
	r := newRig(t, repos(`{"name": "files", "format": "raw", "kind": "hosted"}`))
	r.trash.Days = 7
	data := blobBytes(3000, 9)
	r.expect(r.put("/repository/files/a/b.bin", data), 201)
	before := r.liveBytes()
	r.expect(r.do("DELETE", "/repository/files/a/b.bin", nil), 204) // a person's delete goes to the trash too
	r.expect(r.get("/repository/files/a/b.bin"), 404)
	r.collect()
	if r.liveBytes() != before {
		t.Fatal("the bytes were freed while the item is in the trash")
	}
	var items struct {
		Items []struct{ ID, Name, Reason string }
		Days  int
	}
	_ = json.Unmarshal(r.expect(r.get("/api/v1/trash"), 200).body, &items)
	if len(items.Items) != 1 || items.Items[0].Name != "a/b.bin" || items.Items[0].Reason != "delete" || items.Days != 7 {
		t.Fatalf("the trash: %+v", items)
	}
	id := items.Items[0].ID
	// Something is written at the path: the restore is refused and the item stays.
	r.expect(r.put("/repository/files/a/b.bin", []byte("new")), 201)
	r.expect(r.do("POST", "/api/v1/trash/"+id+"/restore", nil), 409)
	r.expect(r.do("DELETE", "/repository/files/a/b.bin", nil), 204)
	// (that delete is in the trash too; the first item is restored)
	r.expect(r.do("POST", "/api/v1/trash/"+id+"/restore", nil), 200)
	if f := r.expect(r.get("/repository/files/a/b.bin"), 200); string(f.body) != string(data) || f.hd.Get("ETag") != `"`+sum(data)+`"` {
		t.Fatal("the restored file")
	}
	r.expect(r.do("POST", "/api/v1/trash/"+id+"/restore", nil), 404)
	// The rest of the trash is emptied by time: after the days, the sweep empties it and the bytes are freed.
	r.advance(8 * 24 * time.Hour)
	if n, err := r.trash.Sweep(context.Background(), 100); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	r.expect(r.do("DELETE", "/repository/files/a/b.bin", nil), 204)
	r.advance(8 * 24 * time.Hour)
	if n, _ := r.trash.Sweep(context.Background(), 100); n != 1 {
		t.Fatalf("sweep of the second: %d", n)
	}
	r.collect()
	if r.liveBytes() != 0 {
		t.Fatalf("after the trash was emptied %d bytes are kept", r.liveBytes())
	}
	// The API is for an admin only, and a repository with no policy has no cleanup.
	anon, _ := http.NewRequest("GET", r.ts.URL+"/api/v1/trash", nil)
	if resp, err := http.DefaultClient.Do(anon); err != nil || resp.StatusCode != 401 {
		t.Fatalf("anonymous: %v", err)
	}
	r.expect(r.get("/api/v1/repositories/files/cleanup"), 404)
}
