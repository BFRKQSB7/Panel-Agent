// Package backup defines the bounded Panel backup envelope. The archive is
// authenticated as a whole; it never executes or extracts arbitrary paths.
package backup

import (
	"archive/zip"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

const (
	Magic            = "panel-agent-backup"
	FormatVersion    = 1
	MaxBundleBytes   = 128 << 20
	MaxArchiveBytes  = 128 << 20
	MaxEntryBytes    = 96 << 20
	MaxExpandedBytes = 128 << 20
	MaxEntries       = 128
	MaxPasswordBytes = 4096
	KDFMemoryKiB     = 32 * 1024
	KDFTime          = 2
	KDFThreads       = 1
)

var (
	ErrInvalidBundle     = errors.New("备份包无效")
	ErrPasswordRequired  = errors.New("备份包需要密码")
	ErrPasswordMismatch  = errors.New("备份包密码错误")
	ErrUnsupportedBundle = errors.New("备份包版本不受支持")
	componentPattern     = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

type ConfigSnapshot struct {
	DataDir      string `json:"data_dir"`
	Listen       string `json:"listen"`
	TLSCertPath  string `json:"tls_cert_path,omitempty"`
	TLSKeyPath   string `json:"tls_key_path,omitempty"`
	TrustProxy   bool   `json:"trust_proxy"`
	CookieSecure bool   `json:"cookie_secure"`
}

type ExtensionFile struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
}

type Manifest struct {
	Magic           string          `json:"magic"`
	Version         int             `json:"version"`
	CreatedAt       string          `json:"created_at"`
	SchemaVersion   int             `json:"schema_version"`
	InstallationID  string          `json:"installation_id"`
	DatabaseSHA256  string          `json:"database_sha256"`
	MasterKeySHA256 string          `json:"master_key_sha256"`
	Config          ConfigSnapshot  `json:"config"`
	Extensions      []ExtensionFile `json:"extensions"`
}

type Input struct {
	Database       []byte
	MasterKey      []byte
	SchemaVersion  int
	InstallationID string
	Config         ConfigSnapshot
	Extensions     map[string][]byte
	Password       string
	CreatedAt      time.Time
}

type Bundle struct {
	Manifest   Manifest
	Database   []byte
	MasterKey  []byte
	Extensions map[string][]byte
}

type envelope struct {
	Magic     string     `json:"magic"`
	Version   int        `json:"version"`
	Encrypted bool       `json:"encrypted"`
	Salt      string     `json:"salt,omitempty"`
	Nonce     string     `json:"nonce,omitempty"`
	KDF       *kdfParams `json:"kdf,omitempty"`
	Checksum  string     `json:"checksum"`
	Payload   string     `json:"payload"`
}

type kdfParams struct {
	Name      string `json:"name"`
	MemoryKiB uint32 `json:"memory_kib"`
	Time      uint32 `json:"time"`
	Threads   uint8  `json:"threads"`
}

func Create(input Input) ([]byte, Manifest, error) {
	if len(input.Database) == 0 || len(input.Database) > MaxEntryBytes || len(input.MasterKey) != 32 || input.SchemaVersion < 1 || strings.TrimSpace(input.InstallationID) == "" {
		return nil, Manifest{}, fmt.Errorf("%w：备份核心文件或元数据无效", ErrInvalidBundle)
	}
	if len([]byte(input.Password)) > MaxPasswordBytes {
		return nil, Manifest{}, fmt.Errorf("%w：密码过长", ErrInvalidBundle)
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now().UTC()
	}
	manifest := Manifest{
		Magic:           Magic,
		Version:         FormatVersion,
		CreatedAt:       input.CreatedAt.UTC().Format(time.RFC3339Nano),
		SchemaVersion:   input.SchemaVersion,
		InstallationID:  input.InstallationID,
		DatabaseSHA256:  digestHex(input.Database),
		MasterKeySHA256: digestHex(input.MasterKey),
		Config:          input.Config,
		Extensions:      []ExtensionFile{},
	}
	keys := make([]string, 0, len(input.Extensions))
	for key, contents := range input.Extensions {
		id, version, err := splitExtensionKey(key)
		if err != nil || len(contents) == 0 || len(contents) > MaxEntryBytes {
			return nil, Manifest{}, fmt.Errorf("%w：扩展资源无效", ErrInvalidBundle)
		}
		keys = append(keys, key)
		manifest.Extensions = append(manifest.Extensions, ExtensionFile{ID: id, Version: version, Path: extensionPath(id, version), SHA256: digestHex(contents)})
	}
	sort.Strings(keys)
	sort.Slice(manifest.Extensions, func(i, j int) bool { return manifest.Extensions[i].Path < manifest.Extensions[j].Path })
	archive, err := makeArchive(manifest, input.Database, input.MasterKey, input.Extensions, keys)
	if err != nil {
		return nil, Manifest{}, err
	}
	if len(archive) > MaxArchiveBytes {
		return nil, Manifest{}, fmt.Errorf("%w：备份压缩包过大", ErrInvalidBundle)
	}
	encoded, err := sealArchive(archive, input.Password)
	if err != nil {
		return nil, Manifest{}, err
	}
	if len(encoded) > MaxBundleBytes {
		return nil, Manifest{}, fmt.Errorf("%w：备份包过大", ErrInvalidBundle)
	}
	return encoded, manifest, nil
}

func Open(data []byte, password string) (Bundle, error) {
	if len(data) == 0 || len(data) > MaxBundleBytes {
		return Bundle{}, fmt.Errorf("%w：包大小无效", ErrInvalidBundle)
	}
	var outer envelope
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&outer); err != nil {
		return Bundle{}, fmt.Errorf("%w：外层格式无效", ErrInvalidBundle)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Bundle{}, fmt.Errorf("%w：外层包含多余数据", ErrInvalidBundle)
	}
	if outer.Magic != Magic {
		return Bundle{}, fmt.Errorf("%w：magic 不匹配", ErrInvalidBundle)
	}
	if outer.Version != FormatVersion {
		return Bundle{}, ErrUnsupportedBundle
	}
	archive, err := base64.RawStdEncoding.DecodeString(outer.Payload)
	if err != nil || len(archive) == 0 || len(archive) > MaxArchiveBytes {
		return Bundle{}, fmt.Errorf("%w：载荷无效", ErrInvalidBundle)
	}
	if outer.Encrypted {
		if strings.TrimSpace(password) == "" {
			return Bundle{}, ErrPasswordRequired
		}
		if len([]byte(password)) > MaxPasswordBytes || outer.KDF == nil || outer.KDF.Name != "argon2id" || outer.KDF.MemoryKiB != KDFMemoryKiB || outer.KDF.Time != KDFTime || outer.KDF.Threads != KDFThreads {
			return Bundle{}, fmt.Errorf("%w：KDF 参数无效", ErrInvalidBundle)
		}
		salt, saltErr := base64.RawStdEncoding.DecodeString(outer.Salt)
		nonce, nonceErr := base64.RawStdEncoding.DecodeString(outer.Nonce)
		if saltErr != nil || len(salt) != 16 || nonceErr != nil || len(nonce) != 12 {
			return Bundle{}, fmt.Errorf("%w：加密参数无效", ErrInvalidBundle)
		}
		key := argon2.IDKey([]byte(password), salt, outer.KDF.Time, outer.KDF.MemoryKiB, outer.KDF.Threads, 32)
		block, blockErr := aes.NewCipher(key)
		if blockErr != nil {
			return Bundle{}, blockErr
		}
		gcm, gcmErr := cipher.NewGCM(block)
		if gcmErr != nil {
			return Bundle{}, gcmErr
		}
		archive, err = gcm.Open(nil, nonce, archive, []byte(Magic))
		if err != nil {
			return Bundle{}, ErrPasswordMismatch
		}
	} else if outer.Salt != "" || outer.Nonce != "" || outer.KDF != nil {
		return Bundle{}, fmt.Errorf("%w：未加密包包含加密参数", ErrInvalidBundle)
	}
	if outer.Checksum != digestHex(archive) {
		return Bundle{}, fmt.Errorf("%w：载荷校验失败", ErrInvalidBundle)
	}
	return parseArchive(archive)
}

func makeArchive(manifest Manifest, database, masterKey []byte, extensions map[string][]byte, keys []string) ([]byte, error) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	entries := []struct {
		name string
		data []byte
	}{
		{name: "manifest.json", data: manifestBytes},
		{name: "panel.db", data: database},
		{name: "master.key", data: masterKey},
	}
	for _, key := range keys {
		id, version, _ := splitExtensionKey(key)
		entries = append(entries, struct {
			name string
			data []byte
		}{name: extensionPath(id, version), data: extensions[key]})
	}
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		header.SetMode(0o600)
		file, err := writer.CreateHeader(header)
		if err != nil {
			return nil, err
		}
		if _, err := file.Write(entry.data); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func sealArchive(archive []byte, password string) ([]byte, error) {
	outer := envelope{Magic: Magic, Version: FormatVersion, Checksum: digestHex(archive)}
	if password == "" {
		outer.Payload = base64.RawStdEncoding.EncodeToString(archive)
		return json.Marshal(outer)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	key := argon2.IDKey([]byte(password), salt, KDFTime, KDFMemoryKiB, KDFThreads, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	outer.Encrypted = true
	outer.Salt = base64.RawStdEncoding.EncodeToString(salt)
	outer.Nonce = base64.RawStdEncoding.EncodeToString(nonce)
	outer.KDF = &kdfParams{Name: "argon2id", MemoryKiB: KDFMemoryKiB, Time: KDFTime, Threads: KDFThreads}
	outer.Payload = base64.RawStdEncoding.EncodeToString(gcm.Seal(nil, nonce, archive, []byte(Magic)))
	return json.Marshal(outer)
}

func parseArchive(archive []byte) (Bundle, error) {
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil || len(reader.File) == 0 || len(reader.File) > MaxEntries {
		return Bundle{}, fmt.Errorf("%w：ZIP 无效", ErrInvalidBundle)
	}
	files := map[string][]byte{}
	var expanded uint64
	for _, file := range reader.File {
		if !safeArchivePath(file.Name) || file.Mode()&0o170000 != 0 || file.UncompressedSize64 > MaxEntryBytes || expanded > MaxExpandedBytes-file.UncompressedSize64 {
			return Bundle{}, fmt.Errorf("%w：ZIP 条目越界", ErrInvalidBundle)
		}
		if _, exists := files[file.Name]; exists {
			return Bundle{}, fmt.Errorf("%w：ZIP 条目重复", ErrInvalidBundle)
		}
		expanded += file.UncompressedSize64
		input, openErr := file.Open()
		if openErr != nil {
			return Bundle{}, fmt.Errorf("%w：ZIP 条目无法打开", ErrInvalidBundle)
		}
		contents, readErr := io.ReadAll(io.LimitReader(input, MaxEntryBytes+1))
		closeErr := input.Close()
		if readErr != nil || closeErr != nil || uint64(len(contents)) != file.UncompressedSize64 || uint64(len(contents)) > MaxEntryBytes {
			return Bundle{}, fmt.Errorf("%w：ZIP 条目无法读取", ErrInvalidBundle)
		}
		files[file.Name] = contents
	}
	manifestBytes, ok := files["manifest.json"]
	if !ok {
		return Bundle{}, fmt.Errorf("%w：缺少 manifest.json", ErrInvalidBundle)
	}
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(manifestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil || manifest.Magic != Magic || manifest.Version != FormatVersion || manifest.SchemaVersion < 1 || strings.TrimSpace(manifest.InstallationID) == "" || manifest.DatabaseSHA256 == "" || manifest.MasterKeySHA256 == "" {
		return Bundle{}, fmt.Errorf("%w：manifest 无效", ErrInvalidBundle)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Bundle{}, fmt.Errorf("%w：manifest 包含多余数据", ErrInvalidBundle)
	}
	database, databaseOK := files["panel.db"]
	masterKey, masterOK := files["master.key"]
	if !databaseOK || !masterOK || len(database) == 0 || len(masterKey) != 32 || digestHex(database) != manifest.DatabaseSHA256 || digestHex(masterKey) != manifest.MasterKeySHA256 {
		return Bundle{}, fmt.Errorf("%w：核心文件校验失败", ErrInvalidBundle)
	}
	extensions := map[string][]byte{}
	expected := map[string]ExtensionFile{}
	for _, item := range manifest.Extensions {
		if !componentPattern.MatchString(item.ID) || !componentPattern.MatchString(item.Version) || item.Path != extensionPath(item.ID, item.Version) || !isHexDigest(item.SHA256) {
			return Bundle{}, fmt.Errorf("%w：扩展清单无效", ErrInvalidBundle)
		}
		if _, exists := expected[item.Path]; exists {
			return Bundle{}, fmt.Errorf("%w：扩展清单重复", ErrInvalidBundle)
		}
		expected[item.Path] = item
		contents, exists := files[item.Path]
		if !exists || digestHex(contents) != item.SHA256 {
			return Bundle{}, fmt.Errorf("%w：扩展资源校验失败", ErrInvalidBundle)
		}
		extensions[item.ID+"@"+item.Version] = contents
	}
	for name := range files {
		if name == "manifest.json" || name == "panel.db" || name == "master.key" {
			continue
		}
		if _, ok := expected[name]; !ok {
			return Bundle{}, fmt.Errorf("%w：存在未声明的 ZIP 条目", ErrInvalidBundle)
		}
	}
	return Bundle{Manifest: manifest, Database: database, MasterKey: masterKey, Extensions: extensions}, nil
}

func splitExtensionKey(key string) (string, string, error) {
	parts := strings.Split(key, "@")
	if len(parts) != 2 || !componentPattern.MatchString(parts[0]) || !componentPattern.MatchString(parts[1]) {
		return "", "", fmt.Errorf("%w：扩展键无效", ErrInvalidBundle)
	}
	return parts[0], parts[1], nil
}

func extensionPath(id, version string) string { return "extensions/" + id + "@" + version + ".json" }

func safeArchivePath(name string) bool {
	return name != "" && !strings.Contains(name, "\\") && !strings.HasPrefix(name, "/") && path.Clean(name) == name && !strings.Contains(name, "../") && !strings.HasSuffix(name, "/")
}

func digestHex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func isHexDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
