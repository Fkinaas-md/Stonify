package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type DebMetadata struct {
	Name        string
	Version     string
	Summary     string
	Description string
	Homepage    string
	Depends     []string
	AerynDeps   []string
}

// OverrideRules stores per-package customizations from stonify-rules.json
type OverrideRules struct {
	ExtraDeps []string `json:"extra_deps"`
	ExecFlags string   `json:"exec_flags"`
}

var depMap = map[string]string{
	"libc6":                      "glibc",
	"libgtk-3-0":                 "gtk3",
	"libglib2.0-0":               "glib2",
	"libasound2":                 "alsa-lib",
	"libnss3":                    "nss",
	"libx11-6":                   "libx11",
	"libxss1":                    "libxscrnsaver",
	"libayatana-appindicator3-1": "libayatana-appindicator",
	"curl":                       "curl",
	"wget":                       "wget",
}

func main() {
	autoInstall := flag.Bool("install", false, "Automatically install the converted package using moss")
	flag.BoolVar(autoInstall, "i", false, "Automatically install the converted package (shorthand)")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Println("Usage: stonify [-i|--install] <package.deb>")
		os.Exit(1)
	}

	debPath, err := filepath.Abs(flag.Arg(0))
	if err != nil {
		fmt.Printf("Error resolving path: %v\n", err)
		os.Exit(1)
	}

	if _, err := os.Stat(debPath); os.IsNotExist(err) {
		fmt.Printf("File not found: %s\n", debPath)
		os.Exit(1)
	}

	workDir, err := os.MkdirTemp(".", ".stonify-work-*")
	if err != nil {
		workDir, err = os.MkdirTemp("", "stonify-work-*")
		if err != nil {
			fmt.Printf("Error creating temporary directory: %v\n", err)
			os.Exit(1)
		}
	}
	defer os.RemoveAll(workDir)

	fmt.Printf("[1/6] Unpacking .deb archive: %s\n", filepath.Base(debPath))
	controlTar, dataTar, err := extractArArchive(debPath, workDir)
	if err != nil {
		fmt.Printf("Error unpacking ar archive: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("[2/6] Reading package metadata & mapping dependencies...")
	meta, err := parseDebControl(controlTar, workDir)
	if err != nil {
		fmt.Printf("Error parsing control file: %v\n", err)
		os.Exit(1)
	}

	rules := loadOverrides(meta.Name)
	if rules != nil && len(rules.ExtraDeps) > 0 {
		meta.AerynDeps = append(meta.AerynDeps, rules.ExtraDeps...)
	}

	if len(meta.AerynDeps) > 0 {
		fmt.Printf("      -> Resolved runtime dependencies: %s\n", strings.Join(meta.AerynDeps, ", "))
	}

	fmt.Println("[3/6] Extracting payload...")
	pkgDir := filepath.Join(workDir, "pkg")
	rootfsDir := filepath.Join(pkgDir, "rootfs")
	if err := os.MkdirAll(rootfsDir, 0755); err != nil {
		fmt.Printf("Error creating rootfs directory: %v\n", err)
		os.Exit(1)
	}
	_ = os.Symlink(filepath.Join("pkg", "rootfs"), filepath.Join(workDir, "rootfs"))

	if err := extractPayload(dataTar, rootfsDir); err != nil {
		fmt.Printf("Error extracting payload: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("[4/6] Stripping Debian cruft & normalizing layout (UsrMerge)...")
	removeDebianCruft(rootfsDir)
	if err := normalizeAndFix(rootfsDir, meta, rules); err != nil {
		fmt.Printf("Error normalizing rootfs: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("[5/6] Generating stone.yaml recipe...")
	recipePath := filepath.Join(workDir, "stone.yaml")
	if err := generateStoneYaml(recipePath, meta); err != nil {
		fmt.Printf("Error generating stone.yaml: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("[6/6] Building .stone package with boulder...")
	stoneFile, err := buildWithBoulder(workDir)
	if err != nil {
		fmt.Printf("Boulder build error: %v\n", err)
		os.Exit(1)
	}

	currentDir, _ := os.Getwd()
	finalStonePath := filepath.Join(currentDir, filepath.Base(stoneFile))
	if err := copyFile(stoneFile, finalStonePath); err != nil {
		fmt.Printf("Error saving output .stone: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\n[Success] Package built: %s\n", filepath.Base(finalStonePath))
	// Remove workDir ahead of moss index to prevent duplicate package release errors
	os.RemoveAll(workDir)

	shouldInstall := *autoInstall
	if !shouldInstall {
		fmt.Print("\nDo you want to install the package now? [y/N]: ")
		reader := bufio.NewReader(os.Stdin)
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(strings.ToLower(input))
		if input == "y" || input == "yes" {
			shouldInstall = true
		}
	}

	if shouldInstall {
		installStonePackage(currentDir, meta.Name)
	}
}

func loadOverrides(pkgName string) *OverrideRules {
	file, err := os.ReadFile("stonify-rules.json")
	if err != nil {
		return nil
	}
	var allRules map[string]OverrideRules
	if err := json.Unmarshal(file, &allRules); err != nil {
		fmt.Printf("[Warning] Failed to parse stonify-rules.json: %v\n", err)
		return nil
	}
	if rule, exists := allRules[pkgName]; exists {
		fmt.Printf("      -> Applied custom overrides for %s\n", pkgName)
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
		return "", "", fmt.Errorf("file is not a valid ar/.deb archive")
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
			return "", "", fmt.Errorf("invalid file size in ar: %w", err)
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
		return "", "", fmt.Errorf("control.tar or data.tar not found in .deb archive")
	}

	return controlTar, dataTar, nil
}

func parseDebControl(controlTarPath, workDir string) (*DebMetadata, error) {
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

	var rawDeps []string
	var translatedDeps []string
	if depStr, ok := fields["Depends"]; ok {
		re := regexp.MustCompile(`\(.*?\)`)
		cleanDepStr := re.ReplaceAllString(depStr, "")
		for _, d := range strings.Split(cleanDepStr, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				rawDeps = append(rawDeps, d)
				if aerynPkg, exists := depMap[d]; exists {
					translatedDeps = append(translatedDeps, aerynPkg)
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
		Name:        name,
		Version:     version,
		Summary:     summary,
		Description: desc,
		Homepage:    homepage,
		Depends:     rawDeps,
		AerynDeps:   finalDeps,
	}, nil
}

func extractPayload(dataTarPath, destDir string) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}
	return runTarExtract(dataTarPath, destDir)
}

func runTarExtract(archivePath, destDir string) error {
	cmd := exec.Command("bsdtar", "--no-same-owner", "-xf", archivePath, "-C", destDir)
	if err := cmd.Run(); err != nil {
		cmd = exec.Command("tar", "--no-same-owner", "-xf", archivePath, "-C", destDir)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("archive extraction failed for %s: %w", archivePath, err)
		}
	}
	return nil
}

func normalizeAndFix(rootfs string, meta *DebMetadata, rules *OverrideRules) error {
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

func generateStoneYaml(path string, meta *DebMetadata) error {
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
        echo "Error: rootfs was not found inside the boulder container!"
        find /mason/recipe -maxdepth 3 2>/dev/null || true
        exit 1
    fi
`
	return os.WriteFile(path, []byte(content), 0644)
}

func buildWithBoulder(workDir string) (string, error) {
	cmd := exec.Command("boulder", "build")
	cmd.Dir = workDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = nil

	if err := cmd.Run(); err != nil {
		return "", err
	}

	matches, err := filepath.Glob(filepath.Join(workDir, "*.stone"))
	if err != nil || len(matches) == 0 {
		return "", fmt.Errorf(".stone file was not created by boulder")
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
	fmt.Println("\n[ moss ] Generating package repository index...")
	cmd := exec.Command("moss", "index")
	cmd.Dir = repoDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()

	indexFile := filepath.Join(repoDir, "stone.index")
	if _, err := os.Stat(indexFile); os.IsNotExist(err) {
		fmt.Printf("Could not find index file: %s\n", indexFile)
		return
	}

	fmt.Println("[ moss ] Updating local repository...")
	exec.Command("sudo", "moss", "repo", "remove", "local").Run()
	repoCmd := exec.Command("sudo", "moss", "repo", "add", "--comment", "Local packages", "local", "file://"+indexFile)
	repoCmd.Stdout = os.Stdout
	repoCmd.Stderr = os.Stderr
	if err := repoCmd.Run(); err != nil {
		fmt.Printf("Error adding moss repository: %v\n", err)
		return
	}

	fmt.Printf("[ moss ] Installing package %s...\n", pkgName)
	installCmd := exec.Command("sudo", "moss", "pkg", "add", "--reinstall", pkgName)
	installCmd.Stdout = os.Stdout
	installCmd.Stderr = os.Stderr
	installCmd.Stdin = os.Stdin
	if err := installCmd.Run(); err != nil {
		fallbackCmd := exec.Command("sudo", "moss", "pkg", "add", pkgName)
		fallbackCmd.Stdout = os.Stdout
		fallbackCmd.Stderr = os.Stderr
		fallbackCmd.Run()
	}

	fmt.Println("[ Desktop ] Refreshing application shortcuts and icon caches...")
	exec.Command("kbuildsycoca6").Run()
	exec.Command("update-desktop-database", "-q").Run()
	exec.Command("gtk-update-icon-cache", "-q", "-t", "-f", "/usr/share/icons/hicolor").Run()
	fmt.Println("[Done] Package successfully installed and ready to launch!")
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