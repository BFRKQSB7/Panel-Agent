// Package extensions implements the deliberately small, declarative protocol
// package boundary. Packages describe data; they never provide executable
// code, templates, paths, or arbitrary Xray JSON.
package extensions

import (
	"archive/zip"
	"bytes"
	"crypto"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"

	"panel-agent.local/panel-agent/internal/protocol"
)

const (
	MaxCompressedBytes   = 1 << 20
	MaxExpandedBytes     = 4 << 20
	MaxPackageEntries    = 32
	MaxPathDepth         = 4
	PackageFileName      = "package.json"
	SupportedCoreVersion = "26.10.21"
)

var (
	packageIDPattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{2,31}$`)
	packageVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	fieldNamePattern      = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

type Package struct {
	Manifest  Manifest  `json:"manifest"`
	Schema    Schema    `json:"schema"`
	Renderers Renderers `json:"renderers"`
}

type Manifest struct {
	ID           string       `json:"id"`
	Version      string       `json:"version"`
	Name         string       `json:"name"`
	Description  string       `json:"description"`
	Core         CoreSupport  `json:"core"`
	Capabilities Capabilities `json:"capabilities"`
}

type CoreSupport struct {
	Kind       string `json:"kind"`
	MinVersion string `json:"min_version"`
}

type Capabilities struct {
	Direct      bool `json:"direct"`
	OneHop      bool `json:"one_hop"`
	URI         bool `json:"uri"`
	Mihomo      bool `json:"mihomo"`
	SingBox     bool `json:"sing_box"`
	Certificate bool `json:"certificate"`
}

type Schema struct {
	Fields []Field `json:"fields"`
}

type Field struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Label    string   `json:"label"`
	Required bool     `json:"required"`
	Secret   bool     `json:"secret"`
	Max      int      `json:"max"`
	Options  []string `json:"options,omitempty"`
}

type Renderers struct {
	Xray     XrayRenderer         `json:"xray"`
	Hysteria *HysteriaRenderer    `json:"hysteria,omitempty"`
	URI      SubscriptionRenderer `json:"uri"`
	Mihomo   SubscriptionRenderer `json:"mihomo"`
}

type HysteriaRenderer struct {
	Kind    string `json:"kind"`
	Version int    `json:"version"`
}

func RendererKind(pkg Package) string {
	if pkg.Renderers.Hysteria != nil {
		return pkg.Renderers.Hysteria.Kind
	}
	return pkg.Renderers.Xray.Kind
}

type XrayRenderer struct {
	Kind     string `json:"kind"`
	Protocol string `json:"protocol"`
	Network  string `json:"network"`
	TLS      bool   `json:"tls"`
}

type SubscriptionRenderer struct {
	Kind    string `json:"kind"`
	Enabled bool   `json:"enabled"`
}

type CompiledNode struct {
	ID             string `json:"id"`
	Version        string `json:"version"`
	Kind           string `json:"kind"`
	SNI            string `json:"sni"`
	CertificatePEM string `json:"certificate_pem"`
	PrivateKeyPEM  string `json:"private_key_pem"`
}

var ErrInvalidPackage = errors.New("扩展包无效")

func ParseJSON(data []byte) (Package, error) {
	var pkg Package
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pkg); err != nil {
		return Package{}, fmt.Errorf("%w：包 JSON 无法解析", ErrInvalidPackage)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Package{}, fmt.Errorf("%w：包 JSON 必须只有一个值", ErrInvalidPackage)
	}
	if err := Validate(pkg); err != nil {
		return Package{}, err
	}
	return pkg, nil
}

func CanonicalJSON(pkg Package) ([]byte, error) {
	if err := Validate(pkg); err != nil {
		return nil, err
	}
	return json.Marshal(pkg)
}

func Validate(pkg Package) error {
	if !packageIDPattern.MatchString(pkg.Manifest.ID) || !packageVersionPattern.MatchString(pkg.Manifest.Version) {
		return fmt.Errorf("%w：ID 或版本格式无效", ErrInvalidPackage)
	}
	if !safeText(pkg.Manifest.Name, 128) || !safeText(pkg.Manifest.Description, 512) {
		return fmt.Errorf("%w：名称或说明无效", ErrInvalidPackage)
	}
	hy2 := pkg.Renderers.Hysteria != nil
	if !hy2 && (pkg.Manifest.Core.Kind != "xray" || !supportedCoreMinVersion(pkg.Manifest.Core.MinVersion)) || hy2 && (pkg.Manifest.Core.Kind != "hysteria" || pkg.Manifest.Core.MinVersion != "2.13.0") {
		return fmt.Errorf("%w：Core 兼容范围无效", ErrInvalidPackage)
	}
	if pkg.Manifest.Capabilities.OneHop || pkg.Manifest.Capabilities.SingBox || !pkg.Manifest.Capabilities.Direct || !pkg.Manifest.Capabilities.URI || !pkg.Manifest.Capabilities.Mihomo || !pkg.Manifest.Capabilities.Certificate {
		return fmt.Errorf("%w：声明了运行时不支持的能力", ErrInvalidPackage)
	}
	if hy2 {
		if pkg.Renderers.Xray != (XrayRenderer{}) || pkg.Renderers.Hysteria.Kind != "hysteria2_tls" || pkg.Renderers.Hysteria.Version != 2 {
			return fmt.Errorf("%w：Hysteria renderer 不在白名单内", ErrInvalidPackage)
		}
	} else if pkg.Renderers.Xray.Kind != "trojan_tls" || pkg.Renderers.Xray.Protocol != "trojan" || pkg.Renderers.Xray.Network != "tcp" || !pkg.Renderers.Xray.TLS {
		return fmt.Errorf("%w：Xray renderer 不在白名单内", ErrInvalidPackage)
	}
	if pkg.Renderers.URI.Kind != RendererKind(pkg) || !pkg.Renderers.URI.Enabled || pkg.Renderers.Mihomo.Kind != RendererKind(pkg) || !pkg.Renderers.Mihomo.Enabled {
		return fmt.Errorf("%w：订阅 renderer 不完整", ErrInvalidPackage)
	}
	if len(pkg.Schema.Fields) != 3 {
		return fmt.Errorf("%w：字段数量不受支持", ErrInvalidPackage)
	}
	seen := map[string]bool{}
	expected := map[string]struct {
		typ, label string
		secret     bool
	}{
		"sni":             {typ: "host", label: "TLS SNI", secret: false},
		"certificate_pem": {typ: "pem", label: "TLS 证书 PEM", secret: true},
		"private_key_pem": {typ: "private_key_pem", label: "TLS 私钥 PEM", secret: true},
	}
	for _, field := range pkg.Schema.Fields {
		if !fieldNamePattern.MatchString(field.Name) || seen[field.Name] || !safeText(field.Label, 128) {
			return fmt.Errorf("%w：字段名或标签无效", ErrInvalidPackage)
		}
		seen[field.Name] = true
		want, ok := expected[field.Name]
		if !ok || !field.Required || field.Type != want.typ || field.Label != want.label || field.Secret != want.secret || field.Max < 1 || field.Max > 65536 || len(field.Options) != 0 {
			return fmt.Errorf("%w：字段 %q 不在白名单内", ErrInvalidPackage, field.Name)
		}
	}
	for name := range expected {
		if !seen[name] {
			return fmt.Errorf("%w：缺少字段 %q", ErrInvalidPackage, name)
		}
	}
	return nil
}

func supportedCoreMinVersion(value string) bool {
	if !packageVersionPattern.MatchString(value) {
		return false
	}
	minParts := strings.Split(value, ".")
	currentParts := strings.Split(SupportedCoreVersion, ".")
	for i := range currentParts {
		min, err := strconv.Atoi(minParts[i])
		if err != nil {
			return false
		}
		current, err := strconv.Atoi(currentParts[i])
		if err != nil {
			return false
		}
		if min < current {
			return true
		}
		if min > current {
			return false
		}
	}
	return true
}

func safeText(value string, max int) bool {
	if strings.TrimSpace(value) == "" || len([]rune(value)) > max || strings.ContainsAny(value, "<>`") {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func ImportZip(data []byte) (Package, error) {
	if len(data) == 0 || len(data) > MaxCompressedBytes {
		return Package{}, fmt.Errorf("%w：压缩包大小超限", ErrInvalidPackage)
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return Package{}, fmt.Errorf("%w：压缩包无法读取", ErrInvalidPackage)
	}
	if len(archive.File) == 0 || len(archive.File) > MaxPackageEntries {
		return Package{}, fmt.Errorf("%w：条目数量超限", ErrInvalidPackage)
	}
	var packageJSON []byte
	packageEntrySeen := false
	var expanded uint64
	for _, file := range archive.File {
		if err := validateArchivePath(file); err != nil {
			return Package{}, err
		}
		if file.FileInfo().IsDir() {
			return Package{}, fmt.Errorf("%w：压缩包不允许目录条目", ErrInvalidPackage)
		}
		if file.Name != PackageFileName {
			return Package{}, fmt.Errorf("%w：只允许 %s", ErrInvalidPackage, PackageFileName)
		}
		if packageEntrySeen {
			return Package{}, fmt.Errorf("%w：包文件重复", ErrInvalidPackage)
		}
		packageEntrySeen = true
		if file.UncompressedSize64 > MaxExpandedBytes || expanded > MaxExpandedBytes-file.UncompressedSize64 {
			return Package{}, fmt.Errorf("%w：展开大小超限", ErrInvalidPackage)
		}
		expanded += file.UncompressedSize64
		reader, err := file.Open()
		if err != nil {
			return Package{}, fmt.Errorf("%w：条目无法打开", ErrInvalidPackage)
		}
		packageJSON, err = io.ReadAll(io.LimitReader(reader, MaxExpandedBytes+1))
		_ = reader.Close()
		if err != nil || len(packageJSON) > MaxExpandedBytes {
			return Package{}, fmt.Errorf("%w：条目无法读取", ErrInvalidPackage)
		}
	}
	if len(packageJSON) == 0 {
		return Package{}, fmt.Errorf("%w：缺少 %s", ErrInvalidPackage, PackageFileName)
	}
	return ParseJSON(packageJSON)
}

func validateArchivePath(file *zip.File) error {
	name := strings.ReplaceAll(file.Name, "\\", "/")
	parts := strings.Split(strings.TrimSuffix(name, "/"), "/")
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsRune(name, 0) || path.IsAbs(name) || len(parts) > MaxPathDepth || len(parts[0]) >= 2 && parts[0][1] == ':' {
		return fmt.Errorf("%w：压缩包路径无效", ErrInvalidPackage)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." || part == "" && name != strings.TrimSuffix(name, "/") {
			return fmt.Errorf("%w：压缩包路径穿越", ErrInvalidPackage)
		}
	}
	mode := file.Mode()
	if mode&os.ModeSymlink != 0 || mode&os.ModeNamedPipe != 0 || mode&os.ModeDevice != 0 || mode&os.ModeCharDevice != 0 {
		return fmt.Errorf("%w：压缩包不允许链接或特殊文件", ErrInvalidPackage)
	}
	return nil
}

func CompileNode(pkg Package, values map[string]string) (CompiledNode, error) {
	if err := Validate(pkg); err != nil {
		return CompiledNode{}, err
	}
	for key := range values {
		if key != "sni" && key != "certificate_pem" && key != "private_key_pem" {
			return CompiledNode{}, fmt.Errorf("%w：存在未知动态字段", ErrInvalidPackage)
		}
	}
	sni, err := protocol.NormalizeHost(values["sni"])
	if err != nil || sni == "" || net.ParseIP(sni) != nil {
		return CompiledNode{}, fmt.Errorf("%w：SNI 必须是域名", ErrInvalidPackage)
	}
	certificate, err := parseCertificate(values["certificate_pem"])
	if err != nil {
		return CompiledNode{}, fmt.Errorf("%w：证书字段无效", ErrInvalidPackage)
	}
	privateKey, err := parsePrivateKey(values["private_key_pem"])
	if err != nil || !samePublicKey(certificate, privateKey) {
		return CompiledNode{}, fmt.Errorf("%w：私钥字段无效", ErrInvalidPackage)
	}
	return CompiledNode{ID: pkg.Manifest.ID, Version: pkg.Manifest.Version, Kind: RendererKind(pkg), SNI: sni, CertificatePEM: values["certificate_pem"], PrivateKeyPEM: values["private_key_pem"]}, nil
}

func parseCertificate(value string) (*x509.Certificate, error) {
	block, rest := pem.Decode([]byte(value))
	if block == nil || len(bytes.TrimSpace(rest)) != 0 || block.Type != "CERTIFICATE" {
		return nil, errors.New("certificate PEM invalid")
	}
	return x509.ParseCertificate(block.Bytes)
}

func parsePrivateKey(value string) (any, error) {
	block, rest := pem.Decode([]byte(value))
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("private key PEM invalid")
	}
	if block.Type == "PRIVATE KEY" {
		return x509.ParsePKCS8PrivateKey(block.Bytes)
	}
	if block.Type == "RSA PRIVATE KEY" {
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	}
	if block.Type == "EC PRIVATE KEY" {
		return x509.ParseECPrivateKey(block.Bytes)
	}
	return nil, errors.New("private key type invalid")
}

func samePublicKey(certificate *x509.Certificate, privateKey any) bool {
	private, ok := privateKey.(interface{ Public() crypto.PublicKey })
	return ok && publicKeyEqual(certificate.PublicKey, private.Public())
}

func publicKeyEqual(a, b any) bool {
	left, err := x509.MarshalPKIXPublicKey(a)
	if err != nil {
		return false
	}
	right, err := x509.MarshalPKIXPublicKey(b)
	return err == nil && bytes.Equal(left, right)
}

func RenderURI(pkg Package, name, host string, port int, password string, node CompiledNode) (string, error) {
	if err := Validate(pkg); err != nil || node.Kind != RendererKind(pkg) {
		return "", fmt.Errorf("%w：URI renderer 不可用", ErrInvalidPackage)
	}
	if node.Kind == "hysteria2_tls" {
		u := url.URL{Scheme: "hysteria2", User: url.User(password), Host: net.JoinHostPort(host, strconv.Itoa(port)), Path: "/", RawQuery: url.Values{"sni": {node.SNI}, "insecure": {"0"}}.Encode(), Fragment: name}
		return u.String(), nil
	}
	u := url.URL{Scheme: "trojan", User: url.User(password), Host: net.JoinHostPort(host, fmt.Sprintf("%d", port)), RawQuery: url.Values{"security": {"tls"}, "sni": {node.SNI}, "allowInsecure": {"0"}, "type": {"tcp"}}.Encode(), Fragment: name}
	return u.String(), nil
}

func RenderMihomo(pkg Package, name, host string, port int, password string, node CompiledNode) (map[string]any, error) {
	if err := Validate(pkg); err != nil || node.Kind != RendererKind(pkg) {
		return nil, fmt.Errorf("%w：Mihomo renderer 不可用", ErrInvalidPackage)
	}
	if node.Kind == "hysteria2_tls" {
		return map[string]any{"name": name, "type": "hysteria2", "server": host, "port": port, "password": password, "sni": node.SNI, "skip-cert-verify": false}, nil
	}
	return map[string]any{"name": name, "type": "trojan", "server": host, "port": port, "password": password, "sni": node.SNI, "tls": true, "udp": false}, nil
}
