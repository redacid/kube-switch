package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/getlantern/systray"
)

const enableDebugLogging = true
const logPrefix = "KubeContextSwitcher(Go)"
const maxContextItems = 30
const maxTooltipLength = 120

const kubectlCmd = "kubectl"

const iconLoading = "icons/icon.png"
const iconDefault = "icons/icon.png"
const iconError = "icons/icon.png"
const iconMain = "icons/icon.png"

const labelLoading = "Завантаження..."
const labelError = "Помилка"
const labelNoContext = "Немає контексту"

var arnRegex = regexp.MustCompile(`^arn:aws:eks:[^:]+:(\d+):cluster\/(.+)$`)

var (
	currentContext     string
	kubeconfigFile     string
	isKubeconfigEnvSet bool
	stateMu            sync.RWMutex

	currentContextItem *systray.MenuItem
	contextMenuItems   []*systray.MenuItem
	menuItemContexts   map[*systray.MenuItem]string
	menuMu             sync.Mutex

	watcher     *fsnotify.Watcher
	watcherDone chan bool
)

func logDebug(format string, v ...interface{}) {
	if enableDebugLogging {
		log.Printf(logPrefix+" [DEBUG]: "+format, v...)
	}
}

func logInfo(format string, v ...interface{}) {
	log.Printf(logPrefix+" [INFO]: "+format, v...)
}

func logWarning(format string, v ...interface{}) {
	log.Printf(logPrefix+" [WARN]: "+format, v...)
}

func logError(format string, v ...interface{}) {
	log.Printf(logPrefix+" [ERROR]: "+format, v...)
}

func runKubectl(args ...string) (string, string, error) {
	cmd := exec.Command(kubectlCmd, args...)
	logDebug("Виконую kubectl: %v", cmd.Args)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	stdoutStr := strings.TrimSpace(stdout.String())
	stderrStr := strings.TrimSpace(stderr.String())
	if err != nil {
		errMsg := fmt.Sprintf("команда '%s' не вдалася: %v. Stderr: %s", strings.Join(cmd.Args, " "), err, stderrStr)
		logDebug("Помилка команди kubectl. Stderr: %s, Error: %v", stderrStr, err)
		return stdoutStr, stderrStr, fmt.Errorf(errMsg)
	}
	logDebug("Команда kubectl успішна. Stdout: %s", stdoutStr)
	if stderrStr != "" {
		logDebug("Команда kubectl успішна зі stderr: %s", stderrStr)
	}
	return stdoutStr, stderrStr, nil
}

func getCurrentContext() (string, error) {
	logDebug("Отримання поточного контексту...")
	stdout, stderr, err := runKubectl("config", "current-context")
	if err != nil {
		if strings.Contains(stderr, "current-context is not set") || stdout == "" {
			logDebug("Поточний контекст не встановлено.")
			return "", nil
		}
		logError("Не вдалося отримати поточний контекст: %v", err)
		return "", fmt.Errorf("не вдалося отримати поточний контекст: %w", err)
	}
	return stdout, nil
}

func getContexts() ([]string, error) {
	logDebug("Отримання всіх контекстів...")
	stdout, _, err := runKubectl("config", "get-contexts", "-o", "name")
	if err != nil {
		logError("Не вдалося отримати контексти: %v", err)
		return nil, fmt.Errorf("не вдалося отримати контексти: %w", err)
	}
	if stdout == "" {
		logInfo("Контексти не знайдено.")
		return []string{}, nil
	}
	contextsRaw := strings.Split(stdout, "\n")
	contexts := make([]string, 0, len(contextsRaw))
	for _, c := range contextsRaw {
		if trimmed := strings.TrimSpace(c); trimmed != "" {
			contexts = append(contexts, trimmed)
		}
	}
	logDebug("Знайдено контекстів: %v", contexts)
	return contexts, nil
}

func switchContext(contextName string) error {
	logDebug("Перемикання на контекст '%s'...", contextName)
	_, stderr, err := runKubectl("config", "use-context", contextName)
	if err != nil {
		logError("Не вдалося перемкнути контекст на '%s': %v", contextName, err)
		return fmt.Errorf("не вдалося перемкнути на '%s': %w. Stderr: %s", contextName, err, stderr)
	}
	logInfo("Перемкнено контекст на '%s'", contextName)
	return nil
}

func getDisplayName(contextName string) string {
	if contextName == "" {
		return labelNoContext
	}
	match := arnRegex.FindStringSubmatch(contextName)
	if len(match) == 3 && match[1] != "" && match[2] != "" {
		return fmt.Sprintf("%s:%s", match[1], match[2])
	}
	const maxLen = 40
	if len(contextName) > maxLen {
		return contextName[:maxLen-3] + "..."
	}
	return contextName
}

func updateSystrayUI() {
	logDebug("Оновлення UI systray (Hide/Show)...")

	stateMu.RLock()
	ctx := currentContext
	cfgFile := kubeconfigFile
	cfgEnvSet := isKubeconfigEnvSet
	stateMu.RUnlock()

	displayName := getDisplayName(ctx)
	systray.SetTitle(displayName)
	setIcon(iconMain)

	tooltip := labelNoContext
	var configPathDesc string
	if cfgEnvSet {
		configPathDesc = fmt.Sprintf("Конфіг (KUBECONFIG):\n%s\n(Авто-оновлення ВИМК.)", cfgFile)
	} else {
		configPathDesc = fmt.Sprintf("Конфіг (Дефолт):\n%s\n(Авто-оновлення УВІМК.)", cfgFile)
	}
	if ctx != "" {
		tooltip = fmt.Sprintf("Поточний: %s\n---\n%s", ctx, configPathDesc)
	} else {
		tooltip = fmt.Sprintf("Контекст не встановлено\n---\n%s", configPathDesc)
	}
	if len(tooltip) > maxTooltipLength {
		tooltip = tooltip[:maxTooltipLength-3] + "..."
	}
	systray.SetTooltip(tooltip)

	if currentContextItem != nil {
		if ctx != "" {
			currentContextItem.SetTitle(fmt.Sprintf("✓ %s", displayName))
			currentContextItem.Check()
			currentContextItem.Show()
		} else {
			currentContextItem.SetTitle(labelNoContext)
			currentContextItem.Uncheck()
			currentContextItem.Show()
		}
	} else {
		logError("currentContextItem is nil during update!")
	}

	allContexts, err := getContexts()
	if err != nil {
		logError("Не вдалося отримати контексти для меню: %v", err)
		menuMu.Lock()
		for _, item := range contextMenuItems {
			item.Hide()
			delete(menuItemContexts, item)
		}
		menuMu.Unlock()
	} else {
		if len(allContexts) > maxContextItems {
			logWarning("Кількість контекстів (%d) перевищує ліміт меню (%d). Деякі не будуть показані.", len(allContexts), maxContextItems)
		}

		menuMu.Lock()
		processedItems := 0
		clear(menuItemContexts)

		for _, c := range allContexts {
			if c == ctx {
				continue
			}
			if processedItems >= maxContextItems {
				break
			}

			item := contextMenuItems[processedItems]
			item.SetTitle(fmt.Sprintf("  %s", getDisplayName(c)))
			item.SetTooltip(fmt.Sprintf("Перемкнути на: %s", c))
			menuItemContexts[item] = c
			item.Show()
			processedItems++
		}

		for i := processedItems; i < len(contextMenuItems); i++ {
			item := contextMenuItems[i]
			item.Hide()
			delete(menuItemContexts, item)
		}
		menuMu.Unlock()
	}
	logDebug("UI Systray оновлено (Hide/Show)")
}

func refreshState() {
	logDebug("Оновлення стану...")
	setIcon(iconLoading)
	systray.SetTitle(labelLoading)
	systray.SetTooltip("Оновлення контексту...")

	newContext, err := getCurrentContext()

	stateMu.Lock()
	refreshNeeded := false

	if err != nil {
		logError("Помилка оновлення поточного контексту: %v", err)
		if currentContext != "" {
			logInfo("Контекст скинуто через помилку оновлення.")
			currentContext = ""
			refreshNeeded = true
		}
		stateMu.Unlock()
		setIcon(iconError)
		systray.SetTitle(labelError)
		systray.SetTooltip(fmt.Sprintf("Помилка оновлення: %v", err))
		updateSystrayUI()
		return
	}

	if newContext != currentContext {
		logInfo("Контекст змінився з '%s' на '%s'", currentContext, newContext)
		currentContext = newContext
		refreshNeeded = true
	} else {
		logDebug("Контекст не змінився ('%s')", currentContext)
		refreshNeeded = true
	}
	stateMu.Unlock()

	if refreshNeeded {
		updateSystrayUI()
	}
}

func handleContextSwitchClick(contextName string) {
	logDebug("Обробка кліку для перемикання на '%s'", contextName)
	setIcon(iconLoading)
	systray.SetTooltip("Перемикання контексту...")

	err := switchContext(contextName)
	if err != nil {
		logError("Помилка перемикання контексту через меню на '%s': %v", contextName, err)
		systray.SetTooltip(fmt.Sprintf("Помилка перемикання: %v", err))
		time.Sleep(3 * time.Second)
	}
	refreshState()
}

func setupFileWatcher() {
	stateMu.RLock()
	cfgFile := kubeconfigFile
	cfgEnvSet := isKubeconfigEnvSet
	stateMu.RUnlock()
	if cfgEnvSet {
		logInfo("Встановлено змінну KUBECONFIG. Моніторинг файлу вимкнено.")
		return
	}
	if cfgFile == "" {
		logError("Неможливо налаштувати моніторинг: шлях до kubeconfig порожній.")
		return
	}
	var err error
	watcher, err = fsnotify.NewWatcher()
	if err != nil {
		logError("Не вдалося створити file watcher: %v", err)
		return
	}
	watchDir := filepath.Dir(cfgFile)
	if _, err := os.Stat(watchDir); os.IsNotExist(err) {
		logError("Директорія для моніторингу '%s' не існує.", watchDir)
		watcher.Close()
		watcher = nil
		return
	}
	err = watcher.Add(watchDir)
	if err != nil {
		logError("Не вдалося додати шлях '%s' до file watcher: %v", watchDir, err)
		watcher.Close()
		watcher = nil
		return
	}
	logInfo("Запущено моніторинг директорії: %s (відстеження змін у %s)", watchDir, filepath.Base(cfgFile))
	watcherDone = make(chan bool)
	go func() {
		debounceTimer := time.NewTimer(time.Hour)
		debounceTimer.Stop()
		const debounceDuration = 750 * time.Millisecond
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					logInfo("Канал подій watcher закрито.")
					return
				}
				logDebug("Подія watcher: Name: %s, Op: %s", event.Name, event.Op)
				stateMu.RLock()
				currentCfgFile := kubeconfigFile
				stateMu.RUnlock()
				if filepath.Clean(event.Name) == filepath.Clean(currentCfgFile) {
					if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) || event.Has(fsnotify.Remove) {
						logDebug("Виявлено релевантну зміну для %s. Перезапуск таймера debounce.", currentCfgFile)
						debounceTimer.Reset(debounceDuration)
					}
				} else if event.Has(fsnotify.Rename) || event.Has(fsnotify.Remove) {
					if filepath.Dir(event.Name) == watchDir {
						logDebug("Виявлено потенційно релевантну зміну (rename/remove) у директорії %s. Перезапуск таймера debounce.", watchDir)
						debounceTimer.Reset(debounceDuration)
					}
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					logInfo("Канал помилок watcher закрито.")
					return
				}
				logError("Помилка watcher: %v", err)
			case <-debounceTimer.C:
				logInfo("Спрацював таймер debounce. Оновлення стану через зміну файлу.")
				refreshState()
			case <-watcherDone:
				logInfo("Зупинка горутини file watcher.")
				watcher.Close()
				debounceTimer.Stop()
				return
			}
		}
	}()
}

func stopFileWatcher() {
	if watcher != nil && watcherDone != nil {
		logInfo("Зупинка file watcher...")
		select {
		case <-watcherDone:
		default:
			close(watcherDone)
		}
		watcher = nil
		watcherDone = nil
		logInfo("File watcher зупинено.")
	}
}

func getKubeconfigFile() string {
	if kf := os.Getenv("KUBECONFIG"); kf != "" {
		logDebug("Використання змінної середовища KUBECONFIG: %s", kf)
		stateMu.Lock()
		isKubeconfigEnvSet = true
		stateMu.Unlock()
		listSeparator := string(filepath.ListSeparator)
		files := strings.Split(kf, listSeparator)
		if len(files) > 0 && strings.TrimSpace(files[0]) != "" {
			return files[0]
		}
		logError("KUBECONFIG встановлено, але список файлів порожній або містить помилки.")
		return ""
	}
	logDebug("KUBECONFIG не встановлено, використання стандартного шляху.")
	stateMu.Lock()
	isKubeconfigEnvSet = false
	stateMu.Unlock()
	home, err := os.UserHomeDir()
	if err != nil {
		logError("Не вдалося отримати домашню директорію користувача: %v", err)
		return ""
	}
	return filepath.Join(home, ".kube", "config")
}

func checkKubectlExists() bool {
	logDebug("Перевірка наявності команди kubectl...")
	_, err := exec.LookPath(kubectlCmd)
	if err != nil {
		logError("Команду kubectl не знайдено в PATH: %v", err)
		return false
	}
	logDebug("kubectl знайдено в PATH.")
	return true
}

func loadIcon(path string) []byte {
	logDebug("Завантаження іконки: %s", path)
	data, err := ioutil.ReadFile(path)
	if err != nil {
		logError("Не вдалося завантажити іконку '%s': %v", path, err)
		return nil
	}
	logDebug("Іконку '%s' завантажено (%d байт).", path, len(data))
	return data
}

func setIcon(path string) {
	iconBytes := loadIcon(path)
	if iconBytes != nil {
		systray.SetIcon(iconBytes)
	} else {
		logError("Не вдалося встановити іконку: дані порожні (%s)", path)
	}
}

func openKubeFolder() {
	stateMu.RLock()
	cfgFile := kubeconfigFile
	stateMu.RUnlock()
	dir := filepath.Dir(cfgFile)
	if dir == "." || dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			logError("Не вдалося отримати home dir для відкриття папки .kube")
			return
		}
		dir = filepath.Join(home, ".kube")
	}
	logDebug("Спроба відкрити папку: %s", dir)
	var cmd *exec.Cmd
	switch goos := runtime.GOOS; goos {
	case "linux":
		cmd = exec.Command("xdg-open", dir)
	case "windows":
		cmd = exec.Command("explorer", dir)
	case "darwin":
		cmd = exec.Command("open", dir)
	default:
		logError("Непідтримувана ОС для відкриття папки: %s", goos)
		return
	}
	err := cmd.Start()
	if err != nil {
		logError("Не вдалося відкрити папку '%s': %v", dir, err)
	}
}

func onReady() {
	logInfo("Systray готовий. Версія Go: %s", runtime.Version())
	systray.SetTitle(labelLoading)
	systray.SetTooltip(labelLoading)
	setIcon(iconLoading)

	menuItemContexts = make(map[*systray.MenuItem]string)

	stateMu.Lock()
	kubeconfigFile = getKubeconfigFile()
	stateMu.Unlock()

	if !checkKubectlExists() {
		systray.SetTitle("Помилка")
		systray.SetTooltip("kubectl не знайдено в PATH")
		setIcon(iconError)
		systray.AddMenuItem("Помилка: kubectl не знайдено", "kubectl command is required")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Вийти", "Exit")
		go func() { <-mQuit.ClickedCh; systray.Quit() }()
		return
	}

	currentContextItem = systray.AddMenuItem(labelLoading, "Поточний контекст Kubernetes")
	systray.AddSeparator()
	contextMenuItems = make([]*systray.MenuItem, 0, maxContextItems)
	for i := 0; i < maxContextItems; i++ {
		item := systray.AddMenuItem(fmt.Sprintf("placeholder_%d", i), "")
		item.Hide()
		contextMenuItems = append(contextMenuItems, item)

		go func(menuItem *systray.MenuItem) {
			for range menuItem.ClickedCh {
				menuMu.Lock()
				contextName, ok := menuItemContexts[menuItem]
				menuMu.Unlock()

				if ok && contextName != "" {
					handleContextSwitchClick(contextName)
				} else {
					logDebug("Клікнуто на пункт меню без призначеного контексту?")
				}
			}
			logDebug("Горутина обробника кліків для пункту меню завершується.")
		}(item)
	}
	systray.AddSeparator()
	mRefresh := systray.AddMenuItem("Оновити", "Перезавантажити список та поточний контекст")
	mOpenFolder := systray.AddMenuItem("Відкрити ~/.kube", "Відкрити стандартну папку конфігурації Kubernetes")
	mQuit := systray.AddMenuItem("Вийти", "Завершити програму")
	go func() {
		for {
			select {
			case <-mRefresh.ClickedCh:
				logDebug("Клік 'Оновити'")
				refreshState()
			case <-mOpenFolder.ClickedCh:
				logDebug("Клік 'Відкрити папку'")
				openKubeFolder()
			case <-mQuit.ClickedCh:
				logInfo("Клік 'Вийти'")
				systray.Quit()
				return
			}
		}
	}()

	refreshState()
	setupFileWatcher()
}

func onExit() {
	logInfo("Вихід з systray...")
	stopFileWatcher()
	logInfo("Очищення завершено.")
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime)
	logInfo("Запуск KubeContextSwitcher(Go)...")
	logInfo("Версія Go: %s", runtime.Version())
	systray.Run(onReady, onExit)
	logInfo("KubeContextSwitcher(Go) завершено.")
}
