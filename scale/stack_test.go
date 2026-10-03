//go:build scale

package scale

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	s3backend "gitdr.io/gitdr/internal/dest/s3"
)

// The object store: MinIO with Object Lock over TLS, behind s3limits, both from compose.yaml.
// Certificates and credentials are made here for each run and live only in the run's temp dir.

type stack struct {
	h       *harness
	project string
	compose string
	env     []string
	s3URL   string // https://127.0.0.1:port, the proxy
	caPath  string
	admin   *http.Client
	user    string
	pass    string
}

func startStack(h *harness) (*stack, error) {
	out, err := h.docker("version", "--format", "{{.Server.Arch}}")
	if err != nil {
		return nil, fmt.Errorf("docker is not available: %w: %s", err, out)
	}
	// s3limits runs in a container, built here for the Docker host's architecture.
	arch := strings.TrimSpace(out)
	if arch != "amd64" && arch != "arm64" {
		return nil, fmt.Errorf("docker server architecture %q, want amd64 or arm64", arch)
	}
	s := &stack{
		h:       h,
		project: "gitdr-scale-" + strings.ToLower(h.runID),
		compose: filepath.Join(h.moduleRoot, "scale", "compose.yaml"),
		user:    "scale" + randomHex(6),
		pass:    randomHex(24),
	}

	certs := filepath.Join(h.root, "certs")
	caPEM, err := writeCerts(certs)
	if err != nil {
		return nil, fmt.Errorf("certificates: %w", err)
	}
	s.caPath = filepath.Join(certs, "ca.pem")
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	s.admin = &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}

	binDir := filepath.Join(h.root, "linux-bin")
	if err := h.goBuild(filepath.Join(binDir, "s3limits"), "linux", arch, "./scale/s3limits", "-tags", "scale"); err != nil {
		return nil, err
	}

	s.env = append(slices.Clone(h.origEnv),
		"SCALE_CERTS="+certs,
		"SCALE_BIN="+binDir,
		"SCALE_MINIO_USER="+s.user,
		"SCALE_MINIO_PASSWORD="+s.pass,
		fmt.Sprintf("SCALE_S3_RATE=%d", h.prof.S3Rate),
	)
	fmt.Fprintf(os.Stderr, "scale: starting compose project %s\n", s.project)
	if out, err := s.composeCmd("up", "--detach", "--quiet-pull"); err != nil {
		s.down()
		return nil, fmt.Errorf("compose up: %w: %s", err, out)
	}
	out, err = s.composeCmd("port", "s3limits", "9443")
	if err != nil {
		s.down()
		return nil, fmt.Errorf("compose port: %w: %s", err, out)
	}
	hostPort := strings.TrimSpace(out)
	if _, port, err := net.SplitHostPort(hostPort); err == nil {
		hostPort = net.JoinHostPort("127.0.0.1", port)
	}
	s.s3URL = "https://" + hostPort

	if err := s.waitHealthy(2 * time.Minute); err != nil {
		logs, _ := s.composeCmd("logs", "--no-color", "--tail", "50")
		s.down()
		return nil, fmt.Errorf("%w\n%s", err, logs)
	}

	// From here on the engine, in this process and in every gitdr it starts, reaches the store
	// through the proxy with these.
	for k, v := range map[string]string{
		"AWS_ACCESS_KEY_ID":     s.user,
		"AWS_SECRET_ACCESS_KEY": s.pass,
		"AWS_CA_BUNDLE":         s.caPath,
	} {
		if err := os.Setenv(k, v); err != nil {
			return nil, err
		}
	}
	// 0 sends a single-chunk body to MinIO as it came, to show what MinIO does with what the
	// engine sends; see s3limits.
	if _, err := s.setRechunk(h.prof.RechunkBytes); err != nil {
		s.down()
		return nil, fmt.Errorf("setting the proxy's re-chunking: %w", err)
	}
	fmt.Fprintf(os.Stderr, "scale: object store at %s (s3limits in front of MinIO, re-chunking at %d bytes)\n", s.s3URL, h.prof.RechunkBytes)
	return s, nil
}

func (h *harness) docker(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	cmd.Env = h.origEnv
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func (s *stack) composeCmd(args ...string) (string, error) {
	full := append([]string{"compose", "--project-name", s.project, "--file", s.compose}, args...)
	cmd := exec.Command("docker", full...)
	cmd.Env = s.env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func (s *stack) down() {
	fmt.Fprintf(os.Stderr, "scale: removing compose project %s\n", s.project)
	if out, err := s.composeCmd("down", "--volumes", "--remove-orphans", "--timeout", "5"); err != nil {
		fmt.Fprintf(os.Stderr, "scale: compose down: %v: %s\n", err, out)
	}
}

func (s *stack) waitHealthy(limit time.Duration) error {
	deadline := time.Now().Add(limit)
	var last error
	for time.Now().Before(deadline) {
		resp, err := s.admin.Get(s.s3URL + "/_s3limits/healthz")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
		}
		last = err
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("the store was not healthy after %s: %v", limit, last)
}

// adminJSON calls the proxy's admin API; out, when given, receives the JSON answer.
func (s *stack) adminJSON(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.s3URL+"/_s3limits/"+path, body)
	if err != nil {
		return err
	}
	resp, err := s.admin.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("s3limits %s %s: %s: %s", method, path, resp.Status, b)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// s3Stats mirrors the proxy's stats document.
type s3Stats struct {
	Ops map[string]struct {
		Count    int64            `json:"count"`
		BytesIn  int64            `json:"bytesIn"`
		BytesOut int64            `json:"bytesOut"`
		Status   map[string]int64 `json:"status"`
	} `json:"ops"`
	Refused      map[string]int64 `json:"refused"`
	BytesResent  int64            `json:"bytesResent"`
	WritesResent int64            `json:"writesResent"`
	OpenUploads  int64            `json:"openUploads"`
}

func (s *stack) stats() (s3Stats, error) {
	var st s3Stats
	err := s.adminJSON(http.MethodGet, "stats", nil, &st)
	return st, err
}

// s3Fault is one fault for the proxy to inject; see scale/s3limits.
type s3Fault struct {
	ID       int    `json:"id,omitempty"`
	Op       string `json:"op,omitempty"`
	Bucket   string `json:"bucket,omitempty"`
	KeyRegex string `json:"keyRegex,omitempty"`
	Nth      int    `json:"nth,omitempty"`
	Times    int    `json:"times,omitempty"`
	Action   string `json:"action"`
	Status   int    `json:"status,omitempty"`
	Fired    int    `json:"fired,omitempty"`
	Holding  int    `json:"holding,omitempty"`
}

func (s *stack) addFault(f s3Fault) (int, error) {
	var out struct{ ID int }
	err := s.adminJSON(http.MethodPost, "faults", f, &out)
	return out.ID, err
}

// waitHeld waits until fault id holds a request, or ctx ends.
func (s *stack) waitHeld(ctx context.Context, id int) error {
	for {
		var fs []s3Fault
		if err := s.adminJSON(http.MethodGet, "faults", nil, &fs); err != nil {
			return err
		}
		for _, f := range fs {
			if f.ID == id && f.Holding > 0 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("nothing reached the hold: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (s *stack) clearFaults() error { return s.adminJSON(http.MethodDelete, "faults", nil, nil) }

// setRechunk sets the chunk size the proxy splits a single-chunk body into, 0 for none, and
// returns the size it had.
func (s *stack) setRechunk(bytes int64) (int64, error) {
	var set map[string]any
	if err := s.adminJSON(http.MethodGet, "settings", nil, &set); err != nil {
		return 0, err
	}
	was, _ := set["rechunkBytes"].(float64)
	set["rechunkBytes"] = bytes
	return int64(was), s.adminJSON(http.MethodPut, "settings", set, nil)
}

// newBucket creates a bucket with Object Lock, as an operator would before the first run. The
// engine itself never creates one.
func (s *stack) newBucket(ctx context.Context, name string) error {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"))
	if err != nil {
		return err
	}
	client := awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		o.BaseEndpoint = aws.String(s.s3URL)
		o.UsePathStyle = true
	})
	_, err = client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(name), ObjectLockEnabledForBucket: aws.Bool(true)})
	if err != nil {
		return fmt.Errorf("create bucket %s: %w", name, err)
	}
	_, err = client.PutObjectLockConfiguration(ctx, &awss3.PutObjectLockConfigurationInput{
		Bucket:                  aws.String(name),
		ObjectLockConfiguration: &s3types.ObjectLockConfiguration{ObjectLockEnabled: s3types.ObjectLockEnabledEnabled},
	})
	if err != nil {
		return fmt.Errorf("object lock on %s: %w", name, err)
	}
	return nil
}

// dest is the engine's own S3 destination on bucket, for reading back what a run wrote.
func (s *stack) dest(ctx context.Context, bucket string) (*s3backend.Backend, error) {
	return s3backend.New(ctx, s3backend.Options{
		Bucket: bucket, Region: "us-east-1", Endpoint: s.s3URL, UsePathStyle: true,
	}, slog.New(slog.DiscardHandler))
}

// bucketName is a bucket for one scenario of this run.
func (h *harness) bucketName(parts ...string) string {
	name := strings.ToLower("scale-" + h.runID + "-" + strings.Join(parts, "-"))
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-")
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// writeCerts makes a throwaway CA and the two server certificates the stack needs, laid out the
// way compose.yaml mounts them, and returns the CA in PEM.
func writeCerts(dir string) ([]byte, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "gitdr scale harness throwaway CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(7 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	leaf := func(names ...string) (certPEM, keyPEM []byte, err error) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		tmpl := &x509.Certificate{
			SerialNumber: serial(),
			Subject:      pkix.Name{CommonName: names[0]},
			DNSNames:     names,
			IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
			NotBefore:    now.Add(-time.Hour),
			NotAfter:     now.Add(7 * 24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
		if err != nil {
			return nil, nil, err
		}
		kder, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, nil, err
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}), nil
	}
	minioCert, minioKey, err := leaf("minio", "localhost")
	if err != nil {
		return nil, err
	}
	proxyCert, proxyKey, err := leaf("s3limits", "localhost", "host.docker.internal")
	if err != nil {
		return nil, err
	}
	// Readable by the containers' users, whoever the bind mount says owns them. The CA and its
	// keys are made for this run and gone when it ends.
	files := map[string][]byte{
		"ca.pem":              caPEM,
		"minio/public.crt":    minioCert,
		"minio/private.key":   minioKey,
		"s3limits/cert.pem":   proxyCert,
		"s3limits/key.pem":    proxyKey,
		"s3limits/ca.pem":     caPEM,
		"minio/CAs/.keep":     nil,
		"s3limits/.generated": []byte("throwaway, made by the scale harness for one run\n"),
	}
	for name, b := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return nil, err
		}
	}
	return caPEM, nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		panic(err)
	}
	return n
}
