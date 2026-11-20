// proxybig is a probe of step 5 of the plan (docs/plan.md): a false registry that serves one big layer slowly, and clients that pull it through a proxy repository of a running godepot. It measures
// what the plan asks for: the first byte reaches the client long before the upstream has sent the whole layer (T16, P1), and many clients that ask one cold layer make one fetch (P2, T15). It is
// not part of the program and not tested; docs/proxy.md records what was run.
//
//	go run ./tools/proxybig -listen 127.0.0.1:18291 -url http://127.0.0.1:18290 -token $T -name up/big -mib 5120 -rate 100 -pid $(pgrep -f 'godepot serve')
//	go run ./tools/proxybig ... -mib 512 -clients 100
//
// godepot must have a proxy repository whose upstream is -listen (with allow_private, the address being a loopback one).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	listen  = flag.String("listen", "127.0.0.1:18291", "where the false registry listens (the upstream of the proxy repository)")
	url     = flag.String("url", "http://127.0.0.1:18290", "the address of godepot")
	token   = flag.String("token", os.Getenv("TOKEN"), "a token with read")
	name    = flag.String("name", "up/big", "<proxy repository>/<image>")
	mib     = flag.Int64("mib", 1024, "the size of the layer, in MiB")
	rate    = flag.Int64("rate", 100, "the speed at which the false registry sends, in MiB a second")
	clients = flag.Int("clients", 1, "how many clients ask the cold layer at once")
	pid     = flag.Int("pid", 0, "the process id of godepot, for its memory")
)

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

func hwm(p int) string {
	if p == 0 {
		return "n/a"
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(p) + "/status")
	if err != nil {
		return "n/a"
	}
	var peak, now string
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "VmHWM:"); ok {
			peak = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(l, "VmRSS:"); ok {
			now = strings.TrimSpace(v)
		}
	}
	return "peak " + peak + ", now " + now
}

// stream writes n pseudo-random bytes (the same for the same seed) to w, at most perSec bytes a second, and hashes them when h is not nil.
func stream(w io.Writer, n int64, seed byte, perSec int64, h io.Writer) error {
	var key [32]byte
	key[0] = seed
	rng := rand.NewChaCha8(key)
	buf := make([]byte, 256<<10)
	start := time.Now()
	var sent int64
	for sent < n {
		k := int64(len(buf))
		if k > n-sent {
			k = n - sent
		}
		rng.Read(buf[:k])
		if h != nil {
			h.Write(buf[:k])
		}
		if w != nil {
			if _, err := w.Write(buf[:k]); err != nil {
				return err
			}
		}
		sent += k
		if perSec > 0 {
			if d := time.Duration(float64(sent)/float64(perSec)*float64(time.Second)) - time.Since(start); d > 0 {
				time.Sleep(d)
			}
		}
	}
	return nil
}

func main() {
	flag.Parse()
	size := *mib << 20
	// The digest of the layer, before anything is served.
	h := sha256.New()
	t0 := time.Now()
	_ = stream(nil, size, 7, 0, h)
	layer := "sha256:" + hex.EncodeToString(h.Sum(nil))
	fmt.Printf("layer of %d MiB, %s (hashed in %s)\n", *mib, layer, time.Since(t0).Round(time.Millisecond))
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	cs := sha256.Sum256(config)
	cdigest := "sha256:" + hex.EncodeToString(cs[:])
	manifest, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cdigest, "size": len(config)},
		"layers": []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": layer, "size": size}},
	})
	ms := sha256.Sum256(manifest)
	var layerGets, otherGets atomic.Int64
	image := strings.TrimPrefix(*name, strings.SplitN(*name, "/", 2)[0]+"/")
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/v2/"+image)
		switch {
		case strings.HasPrefix(p, "/manifests/"):
			otherGets.Add(1)
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", "sha256:"+hex.EncodeToString(ms[:]))
			w.Header().Set("Content-Length", strconv.Itoa(len(manifest)))
			if r.Method != http.MethodHead {
				_, _ = w.Write(manifest)
			}
		case p == "/blobs/"+cdigest:
			otherGets.Add(1)
			_, _ = w.Write(config)
		case p == "/blobs/"+layer:
			layerGets.Add(1)
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			if r.Method == http.MethodHead {
				return
			}
			_ = stream(w, size, 7, *rate<<20, nil)
		default:
			http.NotFound(w, r)
		}
	})
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		die("listen: %v", err)
	}
	go func() { _ = (&http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}).Serve(ln) }()

	get := func(path string) (*http.Response, error) {
		req, _ := http.NewRequestWithContext(context.Background(), "GET", *url+path, nil)
		req.Header.Set("Authorization", "Bearer "+*token)
		return http.DefaultClient.Do(req)
	}
	resp, err := get("/v2/" + *name + "/manifests/v")
	if err != nil || resp.StatusCode != 200 {
		die("manifest: %v %v", err, resp)
	}
	resp.Body.Close()
	fmt.Printf("memory of godepot before: %s\n", hwm(*pid))

	var retries atomic.Int64
	pull := func() (first, total time.Duration, ok bool) {
		start := time.Now()
		var resp *http.Response
		var err error
		// A client that is told "503, try again in 5 seconds" does: godepot limits the downloads at once (P11).
		for try := 0; ; try++ {
			resp, err = get("/v2/" + *name + "/blobs/" + layer)
			if err != nil || resp.StatusCode != 503 || try == 60 {
				break
			}
			wait, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			resp.Body.Close()
			retries.Add(1)
			time.Sleep(time.Duration(wait) * time.Second)
		}
		if err != nil || resp.StatusCode != 200 {
			return 0, 0, false
		}
		defer resp.Body.Close()
		sum := sha256.New()
		buf := make([]byte, 1<<20)
		n, err := resp.Body.Read(buf)
		first = time.Since(start)
		sum.Write(buf[:n])
		rest, err2 := io.Copy(sum, resp.Body)
		_ = err
		total = time.Since(start)
		return first, total, err2 == nil && int64(n)+rest == size && "sha256:"+hex.EncodeToString(sum.Sum(nil)) == layer
	}

	if *clients <= 1 {
		first, total, ok := pull()
		fmt.Printf("cold pull: first byte after %s, the whole layer after %s, digest equal: %v; the false registry was asked for the layer %d time(s); memory of godepot: %s\n",
			first.Round(time.Millisecond), total.Round(time.Millisecond), ok, layerGets.Load(), hwm(*pid))
		first2, total2, ok2 := pull()
		fmt.Printf("second pull (the cache): first byte after %s, the whole layer after %s, digest equal: %v; the false registry was asked for the layer %d time(s) in all; memory of godepot: %s\n",
			first2.Round(time.Millisecond), total2.Round(time.Millisecond), ok2, layerGets.Load(), hwm(*pid))
		if !ok || !ok2 || layerGets.Load() != 1 {
			os.Exit(1)
		}
		return
	}
	var wg sync.WaitGroup
	var good, bad atomic.Int64
	start := time.Now()
	for i := 0; i < *clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, ok := pull(); ok {
				good.Add(1)
			} else {
				bad.Add(1)
			}
		}()
	}
	wg.Wait()
	fmt.Printf("%d clients asked the cold layer: %d got the whole layer with the right digest, %d did not (%d answers of 503 with Retry-After were retried); the false registry was asked for the layer %d time(s); all done after %s; memory of godepot: %s\n",
		*clients, good.Load(), bad.Load(), retries.Load(), layerGets.Load(), time.Since(start).Round(time.Millisecond), hwm(*pid))
	if bad.Load() != 0 || layerGets.Load() != 1 {
		os.Exit(1)
	}
}
