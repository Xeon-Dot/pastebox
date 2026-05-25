package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	pastebox "pastebox/internal"
)

type config struct {
	StorageMode string
	ListenAddr  string
	DataDir     string
	ExpireDays  int
	DBDSN       string
	DBCompress  string
}

func loadConfig(path string) (*config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	cfg := &config{
		StorageMode: "local",
		ListenAddr:  ":8080",
		DataDir:     "./data",
		ExpireDays:  30,
		DBCompress:  "zstd",
	}

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "//") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.ToUpper(strings.TrimSpace(parts[0]))
		val := strings.TrimSpace(parts[1])

		switch key {
		case "STORAGE_MODE":
			cfg.StorageMode = val
		case "LISTEN_ADDR":
			cfg.ListenAddr = val
		case "DATA_DIR":
			cfg.DataDir = val
		case "EXPIRE_DAYS":
			if days, err := strconv.Atoi(val); err == nil {
				cfg.ExpireDays = days
			}
		case "DB_DSN":
			cfg.DBDSN = val
		case "DB_COMPRESSION_ALGORITHM":
			cfg.DBCompress = val
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return cfg, nil
}

type app struct {
	store pastebox.Storage
	index *template.Template
}

func main() {
	// 기본값 설정
	listenAddr := getenv("LISTEN_ADDR", ":8080")
	dataDir := getenv("DATA_DIR", "/paste-data")
	expireDays := getenvInt("EXPIRE_DAYS", 30)
	storageMode := "local"
	dbDSN := ""
	dbCompress := "zstd"

	// config.conf 로드 시도
	cfg, err := loadConfig("config.conf")
	if err == nil {
		if cfg.ListenAddr != "" {
			listenAddr = cfg.ListenAddr
		}
		if cfg.DataDir != "" {
			dataDir = cfg.DataDir
		}
		if cfg.ExpireDays > 0 {
			expireDays = cfg.ExpireDays
		}
		if cfg.StorageMode != "" {
			storageMode = strings.ToLower(cfg.StorageMode)
		}
		dbDSN = cfg.DBDSN
		if cfg.DBCompress != "" {
			dbCompress = cfg.DBCompress
		}
		log.Println("설정 파일(config.conf)이 성공적으로 로드되었습니다.")
	} else {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("설정 파일(config.conf) 로드 실패 (환경 변수 모드로 실행): %v", err)
		} else {
			log.Println("설정 파일이 발견되지 않아 환경 변수 기반으로 구동합니다.")
		}
	}

	var store pastebox.Storage
	if storageMode == "db" {
		if dbDSN == "" {
			log.Fatal("DB 모드 실행을 위해 config.conf 내 DB_DSN 설정이 필요합니다.")
		}
		log.Printf("DB 모드로 시작합니다. DSN=%s, 압축=%s", dbDSN, dbCompress)
		store, err = pastebox.NewDBStore(dbDSN, time.Duration(expireDays)*24*time.Hour, dbCompress)
		if err != nil {
			log.Fatalf("DB 연결 및 초기화 실패: %v", err)
		}
	} else {
		log.Printf("로컬 스토리지 모드로 시작합니다. 경로=%s", dataDir)
		store, err = pastebox.NewLocalStore(dataDir, time.Duration(expireDays)*24*time.Hour)
		if err != nil {
			log.Fatalf("로컬 스토리지 초기화 실패: %v", err)
		}
	}
	defer store.Close()

	indexTemplate, err := template.ParseFiles("templates/index.html")
	if err != nil {
		indexTemplate = template.Must(template.New("index").Parse(fallbackIndexHTML))
	}

	a := &app{
		store: store,
		index: indexTemplate,
	}

	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()

		for {
			if err := store.CleanupExpired(); err != nil {
				log.Printf("cleanup failed: %v", err)
			}
			<-ticker.C
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handle)

	log.Printf("pastebox listening on %s", listenAddr)

	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatal(err)
	}
}

func (a *app) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		switch r.Method {
		case http.MethodGet:
			a.indexHandler(w, r)
		case http.MethodPost, http.MethodPut:
			a.uploadHandler(w, r)
		default:
			http.Error(w, "허용되지 않은 메서드입니다.", http.StatusMethodNotAllowed)
		}
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "허용되지 않은 메서드입니다.", http.StatusMethodNotAllowed)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/")
	if strings.Contains(id, "/") || id == "" {
		http.NotFound(w, r)
		return
	}

	if token := r.URL.Query().Get("delete"); token != "" {
		a.deleteHandler(w, r, id, token)
		return
	}

	a.viewHandler(w, r, id)
}

func (a *app) indexHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	data := map[string]string{
		"BaseURL": requestBaseURL(r),
	}

	if err := a.index.Execute(w, data); err != nil {
		http.Error(w, "템플릿 에러", http.StatusInternalServerError)
	}
}

func (a *app) uploadHandler(w http.ResponseWriter, r *http.Request) {
	var reader io.Reader
	contentType := r.Header.Get("Content-Type")

	if strings.HasPrefix(strings.ToLower(contentType), "multipart/form-data") {
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			http.Error(w, "유효하지 않은 multipart 폼입니다.", http.StatusBadRequest)
			return
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "file 필드가 누락되었습니다.", http.StatusBadRequest)
			return
		}
		defer file.Close()

		reader = file

		if header != nil {
			if detected := mime.TypeByExtension(strings.ToLower(filepath.Ext(header.Filename))); detected != "" {
				contentType = detected
			} else {
				contentType = "application/octet-stream"
			}
		}
	} else {
		reader = r.Body
		if strings.TrimSpace(contentType) == "" {
			contentType = "text/plain; charset=utf-8"
		}
	}

	usePassword := strings.EqualFold(strings.TrimSpace(r.Header.Get("usepassword")), "true")
	permanent := strings.EqualFold(strings.TrimSpace(r.Header.Get("data-policy")), "permanent")

	meta, password, deleteToken, err := a.store.Create(reader, contentType, usePassword, permanent)
	if err != nil {
		log.Printf("upload failed: %v", err)
		http.Error(w, "업로드 실패", http.StatusInternalServerError)
		return
	}

	url := strings.TrimRight(requestBaseURL(r), "/") + "/" + meta.ID

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	fmt.Fprintf(w, "주소: %s\n", url)

	if !strings.EqualFold(meta.DataPolicy, "permanent") && !meta.ExpiresAt.IsZero() {
		fmt.Fprintf(w, "만료일: %s\n", meta.ExpiresAt.Format(time.RFC3339))
	}

	if password != "" {
		fmt.Fprintf(w, "비밀번호: %s\n", password)
	}

	fmt.Fprintf(w, "삭제링크: %s?delete=%s\n", url, deleteToken)
}

func (a *app) deleteHandler(w http.ResponseWriter, r *http.Request, id string, token string) {
	if r.Method == http.MethodHead {
		http.Error(w, "허용되지 않은 메서드입니다.", http.StatusMethodNotAllowed)
		return
	}

	if err := a.store.Delete(id, token); err != nil {
		if errors.Is(err, pastebox.ErrInvalidDeleteToken) {
			log.Printf("delete denied: id=%s remote=%s", id, r.RemoteAddr)
			http.Error(w, "삭제 토큰이 누락되었거나 유효하지 않습니다.", http.StatusUnauthorized)
			return
		}

		http.NotFound(w, r)
		return
	}

	log.Printf("deleted: id=%s remote=%s", id, r.RemoteAddr)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "삭제되었습니다.")
}

func (a *app) viewHandler(w http.ResponseWriter, r *http.Request, id string) {
	password := r.URL.Query().Get("password")
	if password == "" {
		password = r.Header.Get("paste-password")
	}

	entry, err := a.store.Open(id, password)
	if err != nil {
		if errors.Is(err, pastebox.ErrInvalidPassword) {
			http.Error(w, "비밀번호가 필요하거나 유효하지 않습니다. ?password=... 쿼리 파라미터나 paste-password 헤더를 사용하세요.", http.StatusUnauthorized)
			return
		}
		http.NotFound(w, r)
		return
	}
	defer entry.File.Close()

	raw := r.URL.Query().Get("raw") == "1"
	browser := isBrowserRequest(r)

	if !raw && browser && isTextEntry(entry) {
		content, err := io.ReadAll(entry.File)
		if err != nil {
			http.Error(w, "파일 읽기 실패", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)

		_ = pasteViewHTML.Execute(w, map[string]any{
			"ID":      entry.Meta.ID,
			"Content": string(content),
		})
		return
	}

	contentType := entry.Meta.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	if isTextEntry(entry) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, entry.Meta.ID))
	}

	if r.Method == http.MethodHead {
		return
	}

	_, _ = io.Copy(w, entry.File)
}

func isBrowserRequest(r *http.Request) bool {
	ua := strings.ToLower(r.UserAgent())
	if strings.HasPrefix(ua, "curl/") || strings.Contains(ua, "wget/") || strings.Contains(ua, "httpie/") {
		return false
	}

	accept := strings.ToLower(r.Header.Get("Accept"))
	return strings.Contains(accept, "text/html") || accept == ""
}

func isTextEntry(entry *pastebox.Entry) bool {
	contentType := strings.ToLower(entry.Meta.ContentType)
	if strings.HasPrefix(contentType, "text/") {
		return true
	}

	if strings.Contains(contentType, "json") ||
		strings.Contains(contentType, "xml") ||
		strings.Contains(contentType, "yaml") ||
		strings.Contains(contentType, "javascript") ||
		strings.Contains(contentType, "x-sh") {
		return true
	}

	pos, _ := entry.File.Seek(0, io.SeekCurrent)

	buf := make([]byte, 4096)
	n, _ := entry.File.Read(buf)
	_, _ = entry.File.Seek(pos, io.SeekStart)

	return looksLikeText(buf[:n])
}

func looksLikeText(buf []byte) bool {
	if len(buf) == 0 {
		return true
	}

	if bytes.IndexByte(buf, 0) >= 0 {
		return false
	}

	if !utf8.Valid(buf) {
		return false
	}

	bad := 0
	for _, b := range buf {
		if b < 0x20 && b != '\n' && b != '\r' && b != '\t' {
			bad++
		}
	}

	return bad == 0
}

func requestBaseURL(r *http.Request) string {
	scheme := "http"
	host := r.Host

	if r.TLS != nil {
		scheme = "https"
	}

	if forwarded := r.Header.Get("Forwarded"); forwarded != "" {
		parts := strings.Split(forwarded, ";")
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(strings.ToLower(part), "proto=") {
				scheme = strings.Trim(strings.TrimPrefix(part, "proto="), `"`)
			}
			if strings.HasPrefix(strings.ToLower(part), "host=") {
				host = strings.Trim(strings.TrimPrefix(part, "host="), `"`)
			}
		}
	}

	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = strings.Split(proto, ",")[0]
		scheme = strings.TrimSpace(scheme)
	}

	if forwardedHost := r.Header.Get("X-Forwarded-Host"); forwardedHost != "" {
		host = strings.Split(forwardedHost, ",")[0]
		host = strings.TrimSpace(host)
	}

	if host == "" {
		host = "localhost"
	}

	return scheme + "://" + host
}

func getenv(key string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func getenvInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	n, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}

	return n
}

var pasteViewHTML = template.Must(template.New("paste").Parse(`<!doctype html>
<html lang="ko">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{ .ID }} - Pastebox</title>
  <script src="https://cdn.tailwindcss.com"></script>
</head>
<body class="min-h-screen bg-[#111111] text-gray-200">
  <main class="mx-auto max-w-5xl px-4 py-8">
    <div class="mb-4 flex items-center justify-between gap-4">
      <h1 class="text-lg font-semibold text-gray-100">Pastebox / {{ .ID }}</h1>
      <div class="flex items-center gap-2">
        <button
          id="copyButton"
          type="button"
          class="rounded-xl border border-gray-700 px-3 py-2 text-sm text-gray-300 hover:bg-gray-900"
          onclick="copyPasteContent()"
        >
          복사
        </button>
        <a class="rounded-xl border border-gray-700 px-3 py-2 text-sm text-gray-300 hover:bg-gray-900" href="?raw=1">원본</a>
      </div>
    </div>
    <pre id="pasteContent" class="overflow-x-auto whitespace-pre-wrap break-words rounded-2xl border border-gray-800 bg-[#111111] p-5 text-sm leading-6 text-gray-200">{{ .Content }}</pre>
  </main>

  <script>
    async function copyPasteContent() {
      const button = document.getElementById("copyButton");
      const content = document.getElementById("pasteContent").innerText;

      try {
        await navigator.clipboard.writeText(content);
        button.innerText = "복사 완료";
      } catch (error) {
        const textarea = document.createElement("textarea");
        textarea.value = content;
        textarea.setAttribute("readonly", "");
        textarea.style.position = "fixed";
        textarea.style.left = "-9999px";
        document.body.appendChild(textarea);
        textarea.select();
        document.execCommand("copy");
        document.body.removeChild(textarea);
        button.innerText = "복사 완료";
      }

      setTimeout(() => {
        button.innerText = "복사";
      }, 1500);
    }
  </script>
</body>
</html>`))

const fallbackIndexHTML = `<!doctype html>
<html lang="ko">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Pastebox</title>
  <script src="https://cdn.tailwindcss.com"></script>
</head>
<body class="min-h-screen bg-[#111111] text-gray-200">
  <main class="mx-auto flex min-h-screen max-w-3xl flex-col justify-center px-6">
    <div class="rounded-2xl border border-gray-800 bg-[#151515] p-8 shadow-2xl">
      <h1 class="text-3xl font-bold text-white">Pastebox</h1>
      <p class="mt-3 text-gray-400">curl 기반 파일 공유 서비스</p>

      <div class="mt-8 space-y-4 text-sm">
        <div class="rounded-xl bg-black/30 p-4">
          <p class="mb-2 font-semibold text-gray-200">텍스트 업로드</p>
          <code class="text-gray-300">echo "hello" | curl -X POST --data-binary @- {{ .BaseURL }}/</code>
        </div>

        <div class="rounded-xl bg-black/30 p-4">
          <p class="mb-2 font-semibold text-gray-200">파일 업로드</p>
          <code class="text-gray-300">curl -F "file=@test.txt" {{ .BaseURL }}/</code>
        </div>

        <div class="rounded-xl bg-black/30 p-4">
          <p class="mb-2 font-semibold text-gray-200">비밀번호 보호</p>
          <code class="text-gray-300">curl -H "usepassword: true" -F "file=@secret.txt" {{ .BaseURL }}/</code>
        </div>

        <div class="rounded-xl bg-black/30 p-4">
          <p class="mb-2 font-semibold text-gray-200">영구 저장</p>
          <code class="text-gray-300">curl -H "data-policy: permanent" -F "file=@test.txt" {{ .BaseURL }}/</code>
        </div>
      </div>
    </div>
  </main>
</body>
</html>`
