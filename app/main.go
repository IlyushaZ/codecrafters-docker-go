package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type nopReader struct{}

func (nopReader) Read(p []byte) (n int, err error) {
	return 0, io.EOF
}

func copyExecutable(src, dst string) error {
	cmd := exec.Command("cp", "--parents", src, dst+"/")
	return cmd.Run()
}

func installImage(ctx context.Context, image, dst string) error {
	name, ref, valid := splitImageName(image)
	if !valid {
		return fmt.Errorf("invalid image: %s", image)
	}

	// we actually don't need manifest here, but only want to receive 401 response,
	// because it will have additional info for requesting an access token
	_, err := fetchManifest(ctx, name, ref, "")
	if err == nil {
		return nil
	}

	unauthErr, ok := err.(unauthorizedError)
	if !ok {
		return fmt.Errorf("fetch manifest: %w", err)
	}

	tokenResp, err := requestToken(ctx, unauthErr.Realm, TokenRequest{
		Service: unauthErr.Service,
		Scope:   unauthErr.Scope,
	})
	if err != nil {
		return fmt.Errorf("request token: %w", err)
	}

	// 2nd attempt - with token set
	man, err := fetchManifest(ctx, name, ref, tokenResp.Token)
	if err != nil {
		return fmt.Errorf("fetch manifest 2: %w", err)
	}

	for _, layer := range man.FSLayers {
		if err := downloadAndUnpackLayer(ctx, name, layer.BlobSum, dst, tokenResp.Token); err != nil {
			return err
		}
	}

	return nil
}

func downloadAndUnpackLayer(ctx context.Context, name, blobSum, dst, token string) error {
	fileName := fmt.Sprintf("layer-%s", blobSum)

	file, err := os.Create(fileName)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer os.Remove(fileName)
	defer file.Close()

	url := fmt.Sprintf("https://registry-1.docker.io/v2/library/%s/blobs/%s", name, blobSum)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	addJWT(req, token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}

	if _, err := io.Copy(file, resp.Body); err != nil {
		return fmt.Errorf("copy to file: %w", err)
	}

	var stderr bytes.Buffer

	cmd := exec.Command("tar", "-xf", fileName, "-C", dst)
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("extract layer %s: %s", blobSum, stderr.String())
	}

	return nil
}

type Manifest struct {
	Name          string      `json:"name"`
	Tag           string      `json:"tag"`
	Architecture  string      `json:"architecture"`
	FSLayers      []FSLayer   `json:"fsLayers"`
	SchemaVersion int64       `json:"schemaVersion"`
	Signatures    []Signature `json:"signatures"`
}

type FSLayer struct {
	BlobSum string `json:"blobSum"`
}

type Signature struct {
	Header    Header `json:"header"`
	Signature string `json:"signature"`
	Protected string `json:"protected"`
}

type Header struct {
	Jwk JWK    `json:"jwk"`
	Alg string `json:"alg"`
}

type JWK struct {
	Crv string `json:"crv"`
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type errorCode int

func (c errorCode) Error() string {
	return "registry api error code " + strconv.Itoa(int(c))
}

type unauthorizedError struct {
	errorCode
	Realm   string
	Service string
	Scope   string
}

func fetchManifest(ctx context.Context, name, ref, token string) (*Manifest, error) {
	url := fmt.Sprintf("https://registry-1.docker.io/v2/library/%s/manifests/%s", name, ref)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	if token != "" {
		addJWT(req, token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, mustParseWWWAuthenticate(resp.Header.Get("Www-Authenticate"))
		}

		return nil, errorCode(resp.StatusCode)
	}

	var m Manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &m, nil
}

// TODO: refactor me!
// Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:samalba/my-app:pull,push"
func mustParseWWWAuthenticate(header string) (err unauthorizedError) {
	err.errorCode = errorCode(http.StatusUnauthorized)
	header = strings.TrimPrefix(header, "Bearer ")

	split := mustSplit(header, ",", 3)
	for _, part := range split {
		splitPart := mustSplit(part, "=", 2)

		switch splitPart[0] {
		case "realm":
			err.Realm, _ = strconv.Unquote(splitPart[1])
		case "service":
			err.Service, _ = strconv.Unquote(splitPart[1])
		case "scope":
			err.Scope, _ = strconv.Unquote(splitPart[1])
		}
	}

	return err
}

func mustSplit(s, delim string, noShorterThan int) []string {
	split := strings.Split(s, delim)
	if len(split) < noShorterThan {
		panic(fmt.Sprintf("split: expected len to be %d, %d given", noShorterThan, len(split)))
	}

	return split
}

func splitImageName(image string) (name, ref string, valid bool) {
	split := strings.Split(image, ":")
	if len(split) != 2 {
		return "", "", false
	}

	return split[0], split[1], true
}

func addJWT(r *http.Request, t string) {
	r.Header.Set("Authorization", "Bearer "+t)
}

type TokenRequest struct {
	Service  string
	ClientID string
	Scope    string
}

type TokenResponse struct {
	Token     string `json:"token"`
	ExpiresIn int64  `json:"expires_in"`
}

func requestToken(ctx context.Context, addr string, req TokenRequest) (*TokenResponse, error) {
	addr = fmt.Sprintf(addr+"?service=%s&scope=%s", req.Service, req.Scope)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, errorCode(resp.StatusCode)
	}

	var tr TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, fmt.Errorf("decode resp: %w", err)
	}

	return &tr, nil
}

// Usage: your_docker.sh run <image> <command> <arg1> <arg2> ...
func main() {
	defer func() {
		if p := recover(); p != nil {
			log.Fatal(p)
		}
	}()

	tmpDir, err := ioutil.TempDir("", "mydocker")
	if err != nil {
		panic(fmt.Sprintf("failed to create tmp dir: %v", err))
	}
	defer os.RemoveAll(tmpDir)

	image := os.Args[2]
	command := os.Args[3]
	args := os.Args[4:len(os.Args)]

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*60)
	defer cancel()

	if err := installImage(ctx, image, tmpDir); err != nil {
		panic(fmt.Sprintf("failed to install image: %v", err))
	}

	if err := copyExecutable(command, tmpDir); err != nil {
		panic(fmt.Sprintf("failed to copy command to tmp dir: %v", err))
	}

	cmd := exec.Command(command, args...)
	cmd.Dir = "/"
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = nopReader{}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Chroot:     tmpDir,
		Cloneflags: syscall.CLONE_NEWPID,
	}

	if err := cmd.Run(); err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			os.Exit(exitError.ExitCode())
		}
	}
}
