///go:build windows

package main

import (
	"archive/zip"
	"bytes"
	"crypto"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var archs = []string{
	"x64",
}

var buildTagRe = regexp.MustCompile(`^v(\d+).(\d+).(\d+)-(\d+)$`)

var signtool string

var now = time.Now().UTC()

func main() {
	{
		CODE_SIGN_PFX, CODE_SIGN_PFX_PASSWORD := os.Getenv("CODE_SIGN_PFX"), os.Getenv("CODE_SIGN_PFX_PASSWORD")
		if CODE_SIGN_PFX == "" || CODE_SIGN_PFX_PASSWORD == "" {
			fmt.Println("Either CODE_SIGN_PFX or CODE_SIGN_PFX_PASSWORD was empty")
			os.Exit(1)
		}
		data, err := base64.StdEncoding.DecodeString(CODE_SIGN_PFX)
		if err != nil {
			fmt.Println("Cannot parse CODE_SIGN_PFX:", err)
			os.Exit(1)
		}
		pfxName := filepath.Join("private", "LiterMC-CodeSign.pfx")
		if err := os.WriteFile(pfxName, data, 0666); err != nil {
			fmt.Println("Cannot save CODE_SIGN_PFX:", err)
			os.Exit(1)
		}
		cmd := exec.Command("certutil", "-f", "-p", CODE_SIGN_PFX_PASSWORD, "-importpfx", pfxName)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Println("Error when loading cert:", err)
			os.Exit(1)
		}
	}

	wixVersion := fmt.Sprintf("1.%d.%d.%d", now.Year()/100, now.Year()%100*400+now.YearDay(), now.Hour()*60+now.Minute())
	fmt.Println("wixVersion:", wixVersion)

	signtool = searchSignTool()
	if signtool == "" {
		fmt.Println("No signtool was found")
		os.Exit(1)
	}
	fmt.Println("signtool path:", signtool)

	os.MkdirAll("output", 0755)

	for _, arch := range archs {
		progName := fmt.Sprintf("litermc-certificate-installer-%s", arch)
		wixDir, err := installWix(arch)
		if err != nil {
			fmt.Println("Error when installing wix", err)
			os.Exit(1)
		}

		tmpDir := filepath.Join("build", "tmp")
		os.RemoveAll(tmpDir)
		if err := os.MkdirAll(tmpDir, 0755); err != nil {
			fmt.Println("Error when preparing app files", err)
			os.Exit(1)
		}

		wsxPath := filepath.Join("installer", "windows")

		if err := run(filepath.Join(wixDir, "candle.exe"),
			"-nologo",
			"-arch", arch,
			"-dBuildVersion="+wixVersion,
			"-dSourceDir=.",
			"-ext", "WixIIsExtension",
			"-out", tmpDir+`\`,
			filepath.Join(wsxPath, "Product.wxs"),
		); err != nil {
			fmt.Println("wix candle exited:", err)
			os.Exit(1)
		}

		msiName := filepath.Join(tmpDir, progName+".unsigned.msi")
		if err := run(filepath.Join(wixDir, "light.exe"),
			"-b", ".",
			"-nologo",
			"-dcl:high",
			"-ext", "WixUIExtension",
			"-ext", "WixUtilExtension",
			"-ext", "WixIIsExtension",
			"-loc", filepath.Join(wsxPath, "Product.Loc-en.wxl"),
			filepath.Join(tmpDir, "Product.wixobj"),
			"-o", msiName,
		); err != nil {
			fmt.Println("wix light exited:", err)
			os.Exit(1)
		}
		if err := signProgram(msiName, filepath.Join("output", progName+".msi")); err != nil {
			fmt.Println("Error when signing:", err)
			os.Exit(1)
		}
	}
}

func searchSignTool() string {
	var (
		signtool string
		version  string
	)
	const baseDir = `C:\Program Files (x86)\Windows Kits\10\bin`
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		panic(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "10.") || !strings.HasSuffix(name, ".0") {
			continue
		}
		if name > version {
			st := filepath.Join(baseDir, name, "x64", "signtool.exe")
			if stat, e := os.Stat(st); e == nil && !stat.IsDir() {
				signtool = st
				version = name
			}
		}
	}
	return signtool
}

func signProgram(in, out string) error {
	if out == "" {
		out = in
	}
	fmt.Printf("Signing %s ...\n", out)
	if in != out {
		if err := osCopy(in, out, 0755); err != nil {
			return err
		}
	}
	if err := run(signtool, "sign", "/sm",
		"/n", "LiterMC-CodeSign",
		// "/tr", "http://timestamp.apple.com/ts01", "/td", "SHA256",
		"/fd", "SHA256", out); err != nil {
		return err
	}
	return nil
}

// See <https://github.com/golang/build/blob/master/internal/installer/windowsmsi/windowsmsi.go#L152>
type wixRelease struct {
	BinaryURL string
	SHA256    string
}

var (
	wixRelease311 = wixRelease{
		BinaryURL: "https://storage.googleapis.com/go-builder-data/wix311-binaries.zip",
		SHA256:    "da034c489bd1dd6d8e1623675bf5e899f32d74d6d8312f8dd125a084543193de",
	}
	wixRelease314 = wixRelease{
		BinaryURL: "https://storage.googleapis.com/go-builder-data/wix314-binaries.zip",
		SHA256:    "34dcbba9952902bfb710161bd45ee2e721ffa878db99f738285a21c9b09c6edb", // WiX v3.14.0.4118 release, SHA 256 of wix314-binaries.zip from https://wixtoolset.org/releases/v3-14-0-4118/.
	}
)

// installWix fetches and installs the wix toolkit
func installWix(arch string) (string, error) {
	var (
		wix  wixRelease
		path string
	)
	switch arch {
	default:
		path = filepath.Join("build", "wix311")
		wix = wixRelease311
		// case "arm", "arm64":
		// 	path = filepath.Join("build", "wix314")
		// 	wix = wixRelease314
	}
	if _, err := os.Stat(filepath.Join(path, "__build_wix_installed")); err == nil {
		fmt.Printf("Cached %s at %s\n", wix.BinaryURL, path)
		return path, nil
	}

	fmt.Printf("Downloading %s to %s\n", wix.BinaryURL, path)
	resp, err := http.Get(wix.BinaryURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Unexpected http status %d", resp.StatusCode)
	}
	hs := crypto.SHA256.New()
	body, err := io.ReadAll(io.TeeReader(resp.Body, hs))
	if err != nil {
		return "", err
	}
	if s := hex.EncodeToString(hs.Sum(nil)); s != wix.SHA256 {
		return "", fmt.Errorf("Hash mismatch, expect %s, got %s", wix.SHA256, s)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), (int64)(len(body)))
	if err != nil {
		return "", err
	}
	for _, f := range zr.File {
		name := filepath.FromSlash(f.Name)
		err := os.MkdirAll(filepath.Join(path, filepath.Dir(name)), 0755)
		if err != nil {
			return "", err
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return "", err
		}
		err = os.WriteFile(filepath.Join(path, name), b, 0644)
		if err != nil {
			return "", err
		}
	}
	os.WriteFile(filepath.Join(path, "__build_wix_installed"), nil, 0644)
	return path, nil
}

func archToMsArch(arch string) string {
	switch arch {
	case "386":
		return "x86"
	case "amd64":
		return "x64"
	case "arm":
		// Historically the installer for the windows/arm port
		// used the same value as for the windows/arm64 port.
		fallthrough
	case "arm64":
		// return "arm64"
		return "x64"
	default:
		panic("unknown arch for windows " + arch)
	}
}

func run(name string, args ...string) error {
	fmt.Printf("$ %s %s\n", name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func osCopy(src, dst string, mode os.FileMode) (err error) {
	srcFd, err := os.Open(src)
	if err != nil {
		return
	}
	defer srcFd.Close()
	dstFd, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return
	}
	_, err = io.Copy(dstFd, srcFd)
	if er := dstFd.Close(); err == nil && er != nil {
		err = er
	}
	if err != nil {
		os.Remove(dst)
		return
	}
	return
}
