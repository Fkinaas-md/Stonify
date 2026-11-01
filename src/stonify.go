package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- ANSI Colors ---
const (
	Reset  = "\033[0m"
	Red    = "\033[31m"
	Green  = "\033[32m"
	Yellow = "\033[33m"
	Cyan   = "\033[36m"
	Bold   = "\033[1m"
)

type DebMetadata struct {
	Name         string
	Version      string
	Architecture string
	Summary      string
	Description  string
	Homepage     string
	AerynDeps    []string
}

type OverrideRules struct {
	ExtraDeps []string `json:"extra_deps"`
	ExecFlags string   `json:"exec_flags"`
	Provides  []string `json:"provides"`
}

var depMap = map[string]string{
	"libc6":                      "glibc",
	"libgtk-3-0":                 "gtk-3",
	"libglib2.0-0":               "glib2",
	"libasound2":                 "alsa-lib",
	"libnss3":                    "nss",
	"libx11-6":                   "libx11",
	"libxss1":                    "libxscrnsaver",
	"libayatana-appindicator3-1": "libayatana-appindicator",
	"curl":                       "curl",
	"wget":                       "wget",
}

// --- CLI Spinner ---
type Spinner struct {
	stopChan chan struct{}
	msg      string
	mu       sync.Mutex
}

func NewSpinner(msg string) *Spinner {
	return &Spinner{msg: msg, stopChan: make(chan struct{})}
}

func (s *Spinner) Start() {
	go func() {
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		i := 0
		for {
			select {
			case <-s.stopChan:
				fmt.Printf("\r\033[K")
				return
			default:
				s.mu.Lock()
				msg := s.msg
				s.mu.Unlock()
				fmt.Printf("\r\033[K%s%s%s %s", Cyan, frames[i%len(frames)], Reset, msg)
				i++
				time.Sleep(80 * time.Millisecond)
			}
		}
	}()
}

func (s *Spinner) Stop(successMsg string) {
	close(s.stopChan)
	time.Sleep(10 * time.Millisecond)
	fmt.Printf("\r\033[K%s✔%s %s\n", Green, Reset, successMsg)
}

func (s *Spinner) Fail(errorMsg string) {
	close(s.stopChan)
	time.Sleep(10 * time.Millisecond)
	fmt.Printf("\r\033[K%s✖%s %s\n", Red, Reset, errorMsg)
}

func (s *Spinner) UpdateMessage(msg string) {
	s.mu.Lock()
	s.msg = msg
	s.mu.Unlock()
}

// --- Main Program ---
func main() {
	autoInstall := flag.Bool("install", false, "Автоматически установить собранный пакет через moss")
	flag.BoolVar(autoInstall, "i", false, "Автоматически установить собранный пакет (сокращение)")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Printf("%s%sИспользование:%s stonify [-i|--install] <файл.deb>\n", Bold, Cyan, Reset)
		os.Exit(1)
	}

	debPath, err := filepath.Abs(flag.Arg(0))
	if err != nil {
		fmt.Printf("%s✖ Ошибка получения пути:%s %v\n", Red, Reset, err)
		os.Exit(1)
	}

	if _, err := os.Stat(debPath); os.IsNotExist(err) {
		fmt.Printf("%s✖ Файл не найден:%s %s\n", Red, Reset, debPath)
		os.Exit(1)
	}

	fmt.Printf("\n%s🚀 Запуск stonify — конвертера Debian -> AerynOS%s\n\n", Bold, Reset)

	workDir, err := os.MkdirTemp(".", ".stonify-work-*")
	if err != nil {
		fmt.Printf("%s✖ Ошибка создания временной папки:%s %v\n", Red, Reset, err)
		os.Exit(1)
	}
	defer os.RemoveAll(workDir)

	sp := NewSpinner(fmt.Sprintf("Распаковка .deb архива: %s", filepath.Base(debPath)))
	sp.Start()
	controlTar, dataTar, err := extractArArchive(debPath, workDir)
	if err != nil {
		sp.Fail(fmt.Sprintf("Ошибка распаковки ar архива: %v", err))
		os.Exit(1)
	}
	sp.Stop("Архив успешно распакован")

	sp = NewSpinner("Чтение метаданных и проверка архитектуры...")
	sp.Start()
	meta, err := parseDebControl(controlTar, workDir, sp)
	if err != nil {
		sp.Fail(fmt.Sprintf("Ошибка чтения control: %v", err))
		os.Exit(1)
	}
	sp.Stop(fmt.Sprintf("Метаданные загружены (%s %s)", meta.Name, meta.Version))

	rules := loadOverrides(meta.Name)
	if rules != nil && len(rules.ExtraDeps) > 0 {
		meta.AerynDeps = append(meta.AerynDeps, rules.ExtraDeps...)
	}
	if len(meta.AerynDeps) > 0 {
		fmt.Printf("   %s↳%s Зависимости для AerynOS: %s%s%s\n", Cyan, Reset, Yellow, strings.Join(meta.AerynDeps, ", "), Reset)
	}

	sp = NewSpinner("Распаковка файловой системы пакета...")
	sp.Start()
	pkgDir := filepath.Join(workDir, "pkg")
	rootfsDir := filepath.Join(pkgDir, "rootfs")
	os.MkdirAll(rootfsDir, 0755)
	_ = os.Symlink(filepath.Join("pkg", "rootfs"), filepath.Join(workDir, "rootfs"))

	if err := extractPayload(dataTar, rootfsDir); err != nil {
		sp.Fail(fmt.Sprintf("Ошибка распаковки данных: %v", err))
		os.Exit(1)
	}
	sp.Stop("Файловая система пакета распакована")

	sp = NewSpinner("Очистка от Debian-мусора и адаптация (UsrMerge)...")
	sp.Start()
	removeDebianCruft(rootfsDir)
	if err := normalizeAndFix(rootfsDir, meta, rules); err != nil {
		sp.Fail(fmt.Sprintf("Ошибка нормализации: %v", err))
		os.Exit(1)
	}
	sp.Stop("Файловая система адаптирована под AerynOS")

	sp = NewSpinner("Генерация сборочного манифеста (stone.yaml)...")
	sp.Start()
	recipePath := filepath.Join(workDir, "stone.yaml")
	if err := generateStoneYaml(recipePath, meta, rules); err != nil {
		sp.Fail(fmt.Sprintf("Ошибка генерации stone.yaml: %v", err))
		os.Exit(1)
	}
	sp.Stop("Манифест stone.yaml успешно сгенерирован")

	sp = NewSpinner("Сборка пакета .stone через boulder (может занять время)...")
	sp.Start()
	stoneFile, err := buildWithBoulder(workDir)
	if err != nil {
		sp.Fail(fmt.Sprintf("Ошибка сборки boulder: %v", err))
		os.Exit(1)
	}
	sp.Stop("Пакет .stone успешно собран!")

	currentDir, _ := os.Getwd()
	finalStonePath := filepath.Join(currentDir, filepath.Base(stoneFile))
	if err := copyFile(stoneFile, finalStonePath); err != nil {
		fmt.Printf("%s✖ Ошибка сохранения .stone:%s %v\n", Red, Reset, err)
		os.Exit(1)
	}

	fmt.Printf("\n%s✨ Готово! Собран пакет: %s%s%s\n", Bold, Green, filepath.Base(finalStonePath), Reset)
	
	os.RemoveAll(workDir)

	shouldInstall := *autoInstall
	if !shouldInstall {
		fmt.Printf("\nХотите установить пакет в систему прямо сейчас? [y/N]: ")
		reader := bufio.NewReader(os.Stdin)
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(strings.ToLower(input))
		if input == "y" || input == "yes" || input == "д" || input == "да" {
			shouldInstall = true
		}
	}

	if shouldInstall {
		installStonePackage(currentDir, meta.Name)
	}
}

func checkArchitecture(debArch string) error {
	if debArch == "all" || debArch == "any" {
		return nil
	}
	archMap := map[string]string{
		"amd64": "amd64",
		"arm64": "arm64",
		"i386":  "386",
		"armhf": "arm",
	}
	expectedArch, ok := archMap[debArch]
	if !ok {
		expectedArch = debArch
	}
	if expectedArch != runtime.GOARCH {
		return fmt.Errorf("Архитектура не совпадает: пакет=%s, система=%s", debArch, runtime.GOARCH)
	}
	return nil
}

func searchMossForPackage(dep string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "moss", "search", dep)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}

	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Searching") || strings.HasPrefix(line, "Repository") {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) > 0 {
			pkgName := parts[0]
			if strings.Contains(pkgName, dep) || strings.Contains(pkgName, strings.TrimPrefix(dep, "lib")) {
				return pkgName
			}
		}
	}
	return ""
}

func loadOverrides(pkgName string) *OverrideRules {
	file, err := os.ReadFile("stonify-rules.json")
	if err != nil {
		return nil
	}
	var allRules map[string]OverrideRules
	if err := json.Unmarshal(file, &allRules); err != nil {
		fmt.Printf("%s⚠ Ошибка парсинга stonify-rules.json:%s %v\n", Yellow, Reset, err)
		return nil
	}
	if rule, exists := allRules[pkgName]; exists {
		fmt.Printf("   %s↳%s Применены пользовательские правила (Overrides)\n", Cyan, Reset)
		return &rule
	}
	return nil
}

func removeDebianCruft(rootfs string) {
	cruft := []string{
		"etc/apt", "etc/cron.d", "etc/cron.daily", "etc/cron.hourly", "etc/cron.monthly",
		"usr/share/doc", "usr/share/man", "usr/share/lintian", "usr/share/menu",
		"lib/systemd", "usr/lib/systemd", "var", "run", "tmp", "dev",
	}
	for _, c := range cruft {
		os.RemoveAll(filepath.Join(rootfs, c))
	}
}

func extractArArchive(debPath, destDir string) (string, string, error) {
	file, err := os.Open(debPath)
	if err != nil {
		return "", "", err
	}
	defer file.Close()

	sig := make([]byte, 8)
	if _, err := io.ReadFull(file, sig); err != nil || string(sig) != "!<arch>\n" {
		return "", "", fmt.Errorf("файл не является корректным ar/.deb архивом")
	}

	var controlTar, dataTar string
	var gnuStringTable []byte

	for {
		header := make([]byte, 60)
		_, err := io.ReadFull(file, header)
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", "", err
		}

		name := strings.TrimSpace(string(header[0:16]))
		sizeStr := strings.TrimSpace(string(header[48:58]))
		size, err := strconv.ParseInt(sizeStr, 10, 64)
		if err != nil {
			return "", "", fmt.Errorf("неверный размер файла в ar: %w", err)
		}

		if name == "//" {
			gnuStringTable = make([]byte, size)
			if _, err := io.ReadFull(file, gnuStringTable); err != nil {
				return "", "", err
			}
			if size%2 != 0 {
				file.Seek(1, io.SeekCurrent)
			}
			continue
		} else if strings.HasPrefix(name, "/") && len(name) > 1 && name[1] >= '0' && name[1] <= '9' {
			offset, _ := strconv.Atoi(strings.TrimSpace(name[1:]))
			if offset < len(gnuStringTable) {
				end := bytes.IndexByte(gnuStringTable[offset:], '/')
				if end != -1 {
					name = string(gnuStringTable[offset : offset+end])
				}
			}
		} else {
			name = strings.TrimSuffix(name, "/")
		}

		outName := ""
		if strings.HasPrefix(name, "control.tar") {
			outName = name
		} else if strings.HasPrefix(name, "data.tar") {
			outName = name
		}

		if outName != "" {
			targetPath := filepath.Join(destDir, outName)
			outF, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
			if err != nil {
				return "", "", err
			}
			if _, err := io.CopyN(outF, file, size); err != nil {
				outF.Close()
				return "", "", err
			}
			outF.Close()

			if strings.HasPrefix(outName, "control.tar") {
				controlTar = targetPath
			} else {
				dataTar = targetPath
			}
		} else {
			if _, err := file.Seek(size, io.SeekCurrent); err != nil {
				return "", "", err
			}
		}

		if size%2 != 0 {
			file.Seek(1, io.SeekCurrent)
		}
	}

	if controlTar == "" || dataTar == "" {
		return "", "", fmt.Errorf("в .deb не найдены control.tar или data.tar")
	}

	return controlTar, dataTar, nil
}

func parseDebControl(controlTarPath, workDir string, sp *Spinner) (*DebMetadata, error) {
	controlDir := filepath.Join(workDir, "control_extracted")
	if err := os.MkdirAll(controlDir, 0755); err != nil {
		return nil, err
	}
	if err := runTarExtract(controlTarPath, controlDir); err != nil {
		return nil, err
	}

	controlFile := filepath.Join(controlDir, "control")
	data, err := os.ReadFile(controlFile)
	if err != nil {
		return nil, err
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	fields := make(map[string]string)
	var currentKey string

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if currentKey != "" {
				fields[currentKey] += " " + strings.TrimSpace(line)
			}
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			currentKey = strings.TrimSpace(parts[0])
			fields[currentKey] = strings.TrimSpace(parts[1])
		}
	}

	name := strings.ToLower(fields["Package"])
	name = regexp.MustCompile(`[^a-z0-9\-\+]`).ReplaceAllString(name, "-")

	version := fields["Version"]
	if idx := strings.Index(version, ":"); idx != -1 {
		version = version[idx+1:]
	}
	version = strings.ReplaceAll(version, "-", ".")
	version = regexp.MustCompile(`[^a-zA-Z0-9\.]`).ReplaceAllString(version, ".")

	arch := strings.ToLower(fields["Architecture"])
	if err := checkArchitecture(arch); err != nil {
		return nil, err
	}

	summary := strings.ReplaceAll(fields["Description"], "\n", " ")
	if summary == "" {
		summary = "Package converted from .deb for AerynOS"
	}
	if len(summary) > 80 {
		summary = summary[:77] + "..."
	}

	desc := fields["Description"]
	if desc == "" {
		desc = summary
	}

	homepage := fields["Homepage"]
	if homepage == "" {
		homepage = "https://aerynos.dev"
	}

	var translatedDeps []string
	if depStr, ok := fields["Depends"]; ok {
		re := regexp.MustCompile(`\(.*?\)`)
		cleanDepStr := re.ReplaceAllString(depStr, "")
		
		depsArray := strings.Split(cleanDepStr, ",")
		for i, d := range depsArray {
			d = strings.TrimSpace(d)
			if d == "" {
				continue
			}

			sp.UpdateMessage(fmt.Sprintf("Поиск зависимостей [%d/%d]: %s", i+1, len(depsArray), d))

			if aerynPkg, exists := depMap[d]; exists {
				translatedDeps = append(translatedDeps, aerynPkg)
			} else {
				foundPkg := searchMossForPackage(d)
				if foundPkg != "" {
					translatedDeps = append(translatedDeps, foundPkg)
				}
			}
		}
	}

	uniqueDeps := make(map[string]bool)
	var finalDeps []string
	for _, d := range translatedDeps {
		if !uniqueDeps[d] {
			uniqueDeps[d] = true
			finalDeps = append(finalDeps, d)
		}
	}

	return &DebMetadata{
		Name:         name,
		Version:      version,
		Architecture: arch,
		Summary:      summary,
		Description:  desc,
		Homepage:     homepage,
		AerynDeps:    finalDeps,
	}, nil
}

func extractPayload(dataTarPath, destDir string) error {
	return runTarExtract(dataTarPath, destDir)
}

func runTarExtract(archivePath, destDir string) error {
	cmd := exec.Command("bsdtar", "--no-same-owner", "-xf", archivePath, "-C", destDir)
	if err := cmd.Run(); err != nil {
		cmd = exec.Command("tar", "--no-same-owner", "-xf", archivePath, "-C", destDir)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("ошибка распаковки архива %s: %w", archivePath, err)
		}
	}
	return nil
}

func normalizeAndFix(rootfs string, meta *DebMetadata, rules *OverrideRules) error {
	multiarchDirs := []string{
		"usr/lib/x86_64-linux-gnu",
		"lib/x86_64-linux-gnu",
		"usr/lib/aarch64-linux-gnu",
		"lib/aarch64-linux-gnu",
	}
	for _, dir := range multiarchDirs {
		mergeDirSafe(filepath.Join(rootfs, dir), filepath.Join(rootfs, "usr", "lib"))
	}

	optDir := filepath.Join(rootfs, "opt")
	if entries, err := os.ReadDir(optDir); err == nil && len(entries) > 0 {
		usrLib := filepath.Join(rootfs, "usr", "lib")
		os.MkdirAll(usrLib, 0755)

		for _, entry := range entries {
			src := filepath.Join(optDir, entry.Name())
			dst := filepath.Join(usrLib, entry.Name())
			moveFileOrDir(src, dst)

			binName := entry.Name()
			usrBin := filepath.Join(rootfs, "usr", "bin")
			os.MkdirAll(usrBin, 0755)
			binPath := filepath.Join(usrBin, strings.ToLower(binName))

			if _, err := os.Stat(binPath); os.IsNotExist(err) {
				execPath := findExecutable(dst, binName)
				if execPath != "" {
					relPath, _ := filepath.Rel(rootfs, execPath)
					relPath = "/" + strings.TrimPrefix(relPath, "/")
					script := fmt.Sprintf("#!/bin/sh\nexec %s \"$@\"\n", relPath)
					os.WriteFile(binPath, []byte(script), 0755)
				}
			}
		}
		os.RemoveAll(optDir)
	}

	mergeDirSafe(filepath.Join(rootfs, "bin"), filepath.Join(rootfs, "usr", "bin"))
	mergeDirSafe(filepath.Join(rootfs, "sbin"), filepath.Join(rootfs, "usr", "bin"))
	mergeDirSafe(filepath.Join(rootfs, "lib"), filepath.Join(rootfs, "usr", "lib"))
	mergeDirSafe(filepath.Join(rootfs, "lib64"), filepath.Join(rootfs, "usr", "lib"))

	// НОВОВВЕДЕНИЕ: Копирование скрытых .so файлов прямо в /usr/lib
	usrLibDir := filepath.Join(rootfs, "usr", "lib")
	var libsToCopy []string

	filepath.Walk(usrLibDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.Contains(info.Name(), ".so") {
			parent := filepath.Dir(path)
			// Если библиотека спрятана в подпапке
			if parent != usrLibDir {
				libsToCopy = append(libsToCopy, path)
			}
		}
		return nil
	})

	// Производим копирование (оно дедуплицируется в moss и позволяет boulder найти ELF-заголовки)
	for _, libPath := range libsToCopy {
		target := filepath.Join(usrLibDir, filepath.Base(libPath))
		if _, err := os.Stat(target); os.IsNotExist(err) {
			copyFile(libPath, target)
		}
	}

	appsDir := filepath.Join(rootfs, "usr", "share", "applications")
	os.MkdirAll(appsDir, 0755)

	var desktopFiles []string
	filepath.Walk(rootfs, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(info.Name(), ".desktop") {
			desktopFiles = append(desktopFiles, path)
		}
		return nil
	})

	for _, dfile := range desktopFiles {
		target := filepath.Join(appsDir, filepath.Base(dfile))
		if dfile != target {
			copyFile(dfile, target)
		}
	}

	fixAllApplicationIcons(rootfs, meta.Name)

	appDesktopFiles, _ := filepath.Glob(filepath.Join(appsDir, "*.desktop"))
	for _, df := range appDesktopFiles {
		patchDesktopFile(df, rootfs, meta.Name, rules)
	}

	return nil
}

func findExecutable(dir, targetName string) string {
	var match string
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if info.Mode()&0111 != 0 {
			if strings.EqualFold(info.Name(), targetName) {
				match = path
				return filepath.SkipAll
			}
			if match == "" {
				match = path
			}
		}
		return nil
	})
	return match
}

func mergeDirSafe(src, dst string) {
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return
	}
	os.MkdirAll(dst, 0755)
	filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil || path == src {
			return nil
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		
		if info.IsDir() {
			os.MkdirAll(target, info.Mode())
		} else {
			moveFileOrDir(path, target)
		}
		return nil
	})
	os.RemoveAll(src)
}

func moveFileOrDir(src, dst string) {
	if err := os.Rename(src, dst); err != nil {
		copyFile(src, dst)
		os.Remove(src)
	}
}

func capitalize(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

func fixAllApplicationIcons(rootfs, pkgName string) {
	hicolorDir := filepath.Join(rootfs, "usr", "share", "icons", "hicolor")
	pixDir := filepath.Join(rootfs, "usr", "share", "pixmaps")
	os.MkdirAll(pixDir, 0755)

	var candidates []string
	filepath.Walk(rootfs, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		name := strings.ToLower(info.Name())
		if strings.HasSuffix(name, ".png") || strings.HasSuffix(name, ".svg") {
			if strings.Contains(name, pkgName) || name == "icon.png" || name == "logo.png" || name == "app.png" {
				candidates = append(candidates, path)
			}
		}
		return nil
	})

	if len(candidates) == 0 {
		return
	}

	bestIcon := candidates[0]
	for _, c := range candidates {
		if strings.HasSuffix(c, ".svg") {
			bestIcon = c
			break
		}
		if strings.Contains(filepath.Base(c), "256") || strings.Contains(filepath.Base(c), "512") {
			bestIcon = c
		}
	}

	names := []string{strings.ToLower(pkgName), capitalize(pkgName), pkgName}
	standardSizes := []string{"32x32", "48x48", "64x64", "128x128", "256x256", "512x512"}

	if strings.HasSuffix(bestIcon, ".svg") {
		targetDir := filepath.Join(hicolorDir, "scalable", "apps")
		os.MkdirAll(targetDir, 0755)
		for _, n := range names {
			copyFile(bestIcon, filepath.Join(targetDir, n+".svg"))
			copyFile(bestIcon, filepath.Join(pixDir, n+".svg"))
		}
	} else {
		for _, size := range standardSizes {
			targetDir := filepath.Join(hicolorDir, size, "apps")
			os.MkdirAll(targetDir, 0755)
			for _, n := range names {
				copyFile(bestIcon, filepath.Join(targetDir, n+".png"))
			}
		}
		for _, n := range names {
			copyFile(bestIcon, filepath.Join(pixDir, n+".png"))
		}
	}
}

func patchDesktopFile(desktopPath, rootfs, pkgName string, rules *OverrideRules) {
	data, err := os.ReadFile(desktopPath)
	if err != nil {
		return
	}

	lines := strings.Split(string(data), "\n")
	var newLines []string
	iconSet := false

	isElectron := false
	if _, err := os.Stat(filepath.Join(rootfs, "usr", "lib", pkgName, "resources", "app.asar")); err == nil {
		isElectron = true
	}

	flagsToAdd := ""
	if rules != nil && rules.ExecFlags != "" {
		flagsToAdd = " " + rules.ExecFlags + " "
	} else if isElectron {
		flagsToAdd = " --ozone-platform-hint=auto --enable-features=WaylandWindowDecorations "
	}

	for _, line := range lines {
		trim := strings.TrimSpace(line)

		if strings.HasPrefix(trim, "Exec=") {
			execVal := strings.TrimPrefix(trim, "Exec=")
			execVal = strings.ReplaceAll(execVal, "/opt/", "/usr/lib/")

			if flagsToAdd != "" && !strings.Contains(execVal, "ozone-platform-hint") {
				if strings.Contains(execVal, " %") {
					idx := strings.Index(execVal, " %")
					execVal = execVal[:idx] + flagsToAdd + execVal[idx:]
				} else {
					execVal = execVal + flagsToAdd
				}
			}
			newLines = append(newLines, "Exec="+execVal)
			continue
		}

		if strings.HasPrefix(trim, "Path=") {
			pathVal := strings.TrimPrefix(trim, "Path=")
			pathVal = strings.ReplaceAll(pathVal, "/opt/", "/usr/lib/")

			fullPath := filepath.Join(rootfs, strings.TrimPrefix(pathVal, "/"))
			if _, err := os.Stat(fullPath); os.IsNotExist(err) {
				continue
			}
			newLines = append(newLines, "Path="+pathVal)
			continue
		}

		if strings.HasPrefix(trim, "Icon=") {
			newLines = append(newLines, "Icon="+strings.ToLower(pkgName))
			iconSet = true
			continue
		}

		newLines = append(newLines, line)
	}

	if !iconSet {
		newLines = append(newLines, "Icon="+strings.ToLower(pkgName))
	}

	os.WriteFile(desktopPath, []byte(strings.Join(newLines, "\n")), 0644)
}

func generateStoneYaml(path string, meta *DebMetadata, rules *OverrideRules) error {
	escapeYAML := func(s string) string {
		s = strings.ReplaceAll(s, "\"", "\\\"")
		s = strings.ReplaceAll(s, "\n", "\n    ")
		return s
	}

	content := fmt.Sprintf(`name        : %s
version     : "%s"
release     : 1
summary     : "%s"
description : |
    %s
homepage    : "%s"
license     :
    - Proprietary
`, meta.Name, meta.Version, escapeYAML(meta.Summary), escapeYAML(meta.Description), escapeYAML(meta.Homepage))

	if len(meta.AerynDeps) > 0 {
		content += "rundeps:\n"
		for _, dep := range meta.AerynDeps {
			content += fmt.Sprintf("    - %s\n", dep)
		}
	}

	if rules != nil && len(rules.Provides) > 0 {
		content += "provides:\n"
		for _, p := range rules.Provides {
			content += fmt.Sprintf("    - %s\n", p)
		}
	}

	content += `install     : |
    mkdir -p %(installroot)
    SRC=""
    for d in "/mason/recipe/pkg/rootfs" "/mason/recipe/rootfs" "%(pkgdir)/rootfs" "%(pkgdir)" "/mason/recipe"; do
        if [ -d "$d/usr" ]; then
            SRC="$d"
            break
        fi
    done
    if [ -z "$SRC" ]; then
        for d in "/mason/recipe/pkg/rootfs" "/mason/recipe/rootfs" "%(pkgdir)/rootfs"; do
            if [ -d "$d" ]; then
                SRC="$d"
                break
            fi
        done
    fi
    if [ -n "$SRC" ]; then
        cp -a "$SRC/." %(installroot)/
    else
        echo "Ошибка: rootfs не найден внутри контейнера boulder!"
        find /mason/recipe -maxdepth 3 2>/dev/null || true
        exit 1
    fi
`
	return os.WriteFile(path, []byte(content), 0644)
}

func buildWithBoulder(workDir string) (string, error) {
	cmd := exec.Command("boulder", "build")
	cmd.Dir = workDir
	cmd.Stdout = nil 
	cmd.Stderr = nil
	cmd.Stdin = nil

	if err := cmd.Run(); err != nil {
		return "", err
	}

	matches, err := filepath.Glob(filepath.Join(workDir, "*.stone"))
	if err != nil || len(matches) == 0 {
		return "", fmt.Errorf(".stone файл не был создан boulder")
	}

	var selected string
	for _, m := range matches {
		if !strings.Contains(m, "-dbginfo-") {
			selected = m
			break
		}
	}
	if selected == "" {
		selected = matches[0]
	}

	return selected, nil
}

func installStonePackage(repoDir, pkgName string) {
	fmt.Printf("\n%s[ moss ]%s Формирование индекса пакетов...\n", Cyan, Reset)
	cmd := exec.Command("moss", "index")
	cmd.Dir = repoDir
	if err := cmd.Run(); err != nil {
		fmt.Printf("%s✖ Ошибка moss index: %v%s\n", Red, err, Reset)
		return
	}

	indexFile := filepath.Join(repoDir, "stone.index")
	if _, err := os.Stat(indexFile); os.IsNotExist(err) {
		fmt.Printf("%s✖ Не удалось найти %s%s\n", Red, indexFile, Reset)
		return
	}

	fmt.Printf("%s[ moss ]%s Обновление локального репозитория...\n", Cyan, Reset)
	exec.Command("sudo", "moss", "repo", "remove", "local").Run()
	if err := exec.Command("sudo", "moss", "repo", "add", "--comment", "Local packages", "local", "file://"+indexFile).Run(); err != nil {
		fmt.Printf("%s✖ Ошибка подключения репозитория: %v%s\n", Red, err, Reset)
		return
	}

	fmt.Printf("%s[ moss ]%s Установка пакета %s...\n", Cyan, Reset, pkgName)
	installCmd := exec.Command("sudo", "moss", "pkg", "add", "--reinstall", pkgName)
	installCmd.Stdout = os.Stdout
	installCmd.Stderr = os.Stderr
	installCmd.Stdin = os.Stdin
	if err := installCmd.Run(); err != nil {
		fallbackCmd := exec.Command("sudo", "moss", "pkg", "add", pkgName)
		fallbackCmd.Stdout = os.Stdout
		fallbackCmd.Stderr = os.Stderr
		if err := fallbackCmd.Run(); err != nil {
			fmt.Printf("\n%s✖ Ошибка установки пакета через moss. Возможно, не хватает зависимостей.%s\n", Red, Reset)
			return
		}
	}

	fmt.Printf("%s[ Desktop ]%s Обновление кэша ярлыков и иконок...\n", Cyan, Reset)
	exec.Command("kbuildsycoca6").Run()
	exec.Command("update-desktop-database", "-q").Run()
	exec.Command("gtk-update-icon-cache", "-q", "-t", "-f", "/usr/share/icons/hicolor").Run()
	
	fmt.Printf("\n%s✔ Пакет %s успешно установлен и готов к запуску!%s\n", Green, pkgName, Reset)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
