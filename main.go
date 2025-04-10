package main

import (
	"bytes"
	"embed" // Імпорт потрібен
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/getlantern/systray"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	// _ "k8s.io/client-go/plugin/pkg/client/auth"
)

//go:embed DejaVuSans.ttf
var embeddedFontData []byte

// Хак для компілятора, щоб гарантовано "бачити" використання пакету embed
var _ embed.FS

const enableDebugLogging = true
const logPrefix = "KubeContextSwitcher(Go-ClientGo-DynIcon)"
const maxContextItems = 30
const maxTooltipLength = 120

const iconSize = 24
const textMaxLen = 4
const fontSizePoints = 10.0

var textColor = color.White
var bgColor = color.Transparent

const kubectlCmd = "kubectl"

const labelLoading = "Завантаження..."
const labelError = "Помилка"
const labelNoContext = "Немає контексту"

var arnRegex = regexp.MustCompile(`^arn:aws:eks:[^:]+:(\d+):cluster\/(.+)$`)

var (
	currentContext     string
	kubeconfigFile     string
	isKubeconfigEnvSet bool
	loadingRules       clientcmd.ClientConfigLoadingRules
	parsedFont         *opentype.Font
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
func logInfo(format string, v ...interface{})    { log.Printf(logPrefix+" [INFO]: "+format, v...) }
func logWarning(format string, v ...interface{}) { log.Printf(logPrefix+" [WARN]: "+format, v...) }
func logError(format string, v ...interface{})   { log.Printf(logPrefix+" [ERROR]: "+format, v...) }

func loadKubeConfig() (*api.Config, string, error) {
	stateMu.RLock()
	rules := loadingRules
	stateMu.RUnlock()
	configPath := rules.GetDefaultFilename()
	logDebug("Спроба завантаження конфігурації з: %s", configPath)
	config, err := clientcmd.LoadFromFile(configPath)
	if err != nil {
		logError("Не вдалося завантажити kubeconfig з '%s': %v", configPath, err)
		if os.IsNotExist(err) {
			return nil, configPath, fmt.Errorf("файл конфігурації не знайдено: %s", configPath)
		}
		return nil, configPath, fmt.Errorf("помилка завантаження '%s': %w", configPath, err)
	}
	logDebug("Kubeconfig '%s' успішно завантажено.", configPath)
	return config, configPath, nil
}

func getCurrentContext() (string, error) {
	logDebug("Отримання поточного контексту через clientcmd...")
	config, _, err := loadKubeConfig()
	if err != nil {
		return "", err
	}
	if config.CurrentContext == "" {
		logDebug("Поле CurrentContext порожнє.")
		return "", nil
	}
	if _, exists := config.Contexts[config.CurrentContext]; !exists {
		logWarning("Поточний контекст '%s' вказано, але його немає у списку!", config.CurrentContext)
		return "", fmt.Errorf("поточний контекст '%s' не знайдено", config.CurrentContext)
	}
	logDebug("Поточний контекст: %s", config.CurrentContext)
	return config.CurrentContext, nil
}

func getContexts() ([]string, error) {
	logDebug("Отримання списку контекстів через clientcmd...")
	config, _, err := loadKubeConfig()
	if err != nil {
		return nil, err
	}
	if len(config.Contexts) == 0 {
		logInfo("У конфігурації не знайдено контекстів.")
		return []string{}, nil
	}
	contexts := make([]string, 0, len(config.Contexts))
	for name := range config.Contexts {
		contexts = append(contexts, name)
	}
	sort.Strings(contexts)
	logDebug("Знайдено та відсортовано контекстів: %v", contexts)
	return contexts, nil
}

func switchContext(contextName string) error {
	logDebug("Перемикання на контекст '%s' через clientcmd...", contextName)
	config, configPath, err := loadKubeConfig()
	if err != nil {
		logError("Не вдалося завантажити конфіг для перемикання: %v", err)
		return fmt.Errorf("неможливо завантажити конфіг: %w", err)
	}
	if _, exists := config.Contexts[contextName]; !exists {
		logError("Спроба перемкнутись на неіснуючий контекст: %s", contextName)
		return fmt.Errorf("контекст '%s' не знайдено", contextName)
	}
	if config.CurrentContext == contextName {
		logInfo("Контекст '%s' вже поточний.", contextName)
		return nil
	}
	config.CurrentContext = contextName
	logDebug("Встановлено CurrentContext = '%s'", contextName)
	logDebug("Збереження змін у файл: %s", configPath)
	err = clientcmd.WriteToFile(*config, configPath)
	if err != nil {
		logError("Не вдалося записати зміни у '%s': %v", configPath, err)
		return fmt.Errorf("помилка збереження конфігурації: %w", err)
	}
	logInfo("Успішно перемкнено на '%s' у %s", contextName, configPath)
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

func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

func generateIcon(text string) ([]byte, error) {
	if parsedFont == nil {
		return nil, errors.New("шрифт не завантажено")
	}

	displayText := strings.ToUpper(truncateString(text, textMaxLen))
	if displayText == "" {
		displayText = "?"
	}

	face, err := opentype.NewFace(parsedFont, &opentype.FaceOptions{
		Size:    fontSizePoints,
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if err != nil {
		logError("Не вдалося створити лице шрифту: %v", err)
		return nil, fmt.Errorf("помилка створення шрифту: %w", err)
	}
	defer face.Close()

	img := image.NewRGBA(image.Rect(0, 0, iconSize, iconSize))
	draw.Draw(img, img.Bounds(), image.Transparent, image.Point{}, draw.Src)

	advance := font.MeasureString(face, displayText)
	metrics := face.Metrics()

	textWidth := int(advance.Ceil())
	textHeight := int((metrics.Ascent + metrics.Descent).Ceil())

	xPos := (iconSize - textWidth) / 2
	yPos := (iconSize / 2) - (textHeight / 2) + int(metrics.Ascent.Ceil())

	point := fixed.Point26_6{
		X: fixed.Int26_6(xPos * 64),
		Y: fixed.Int26_6(yPos * 64),
	}

	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(textColor),
		Face: face,
		Dot:  point,
	}
	d.DrawString(displayText)
	logDebug("Намальовано текст '%s' на іконці %dx%d", displayText, iconSize, iconSize)

	var buf bytes.Buffer
	err = png.Encode(&buf, img)
	if err != nil {
		logError("Не вдалося закодувати іконку в PNG: %v", err)
		return nil, fmt.Errorf("помилка кодування PNG: %w", err)
	}

	return buf.Bytes(), nil
}

func updateSystrayUI() {
	logDebug("Оновлення UI systray (Client-Go / DynIcon)...")

	stateMu.RLock()
	ctx := currentContext
	cfgFile := kubeconfigFile
	cfgEnvSet := isKubeconfigEnvSet
	stateMu.RUnlock()

	displayName := getDisplayName(ctx)
	systray.SetTitle(displayName)

	var iconBytes []byte
	var genErr error
	if ctx != "" {
		iconBytes, genErr = generateIcon(ctx)
	} else {
		iconBytes, genErr = generateIcon(labelNoContext)
	}

	if genErr != nil {
		logError("Помилка генерації динамічної іконки: %v", genErr)
	}

	if iconBytes != nil {
		systray.SetIcon(iconBytes)
	} else {
		logWarning("Згенеровані байти іконки порожні.")
	}

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
		logError("Не вдалося отримати контексти для меню (client-go): %v", err)
		menuMu.Lock()
		for _, item := range contextMenuItems {
			item.Hide()
			delete(menuItemContexts, item)
		}
		menuMu.Unlock()
	} else {
		if len(allContexts) > maxContextItems {
			logWarning("Кількість контекстів (%d) перевищує ліміт меню (%d).", len(allContexts), maxContextItems)
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
	logDebug("UI Systray оновлено (Client-Go / DynIcon)")
}

func refreshState() {
	logDebug("Оновлення стану (client-go / DynIcon)...")
	loadingIconBytes, _ := generateIcon("...")
	if loadingIconBytes != nil {
		systray.SetIcon(loadingIconBytes)
	}
	systray.SetTitle(labelLoading)
	systray.SetTooltip("Оновлення контексту...")

	newContext, err := getCurrentContext()

	stateMu.Lock()
	refreshNeeded := false

	if err != nil {
		logError("Помилка оновлення поточного контексту (client-go): %v", err)
		if currentContext != "" {
			logInfo("Контекст скинуто через помилку.")
			currentContext = ""
			refreshNeeded = true
		}
		stateMu.Unlock()
		errorIconBytes, _ := generateIcon("ERR")
		if errorIconBytes != nil {
			systray.SetIcon(errorIconBytes)
		}
		systray.SetTitle(labelError)
		systray.SetTooltip(fmt.Sprintf("Помилка: %v", err))
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
	logDebug("Обробка кліку для перемикання на '%s' (client-go / DynIcon)", contextName)
	loadingIconBytes, _ := generateIcon("...")
	if loadingIconBytes != nil {
		systray.SetIcon(loadingIconBytes)
	}
	systray.SetTooltip("Перемикання контексту...")

	err := switchContext(contextName)
	if err != nil {
		logError("Помилка перемикання контексту (client-go) на '%s': %v", contextName, err)
		systray.SetTooltip(fmt.Sprintf("Помилка: %v", err))
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
		logInfo("KUBECONFIG встановлено. Моніторинг вимкнено.")
		return
	}
	if cfgFile == "" {
		logError("Неможливо моніторити: шлях порожній.")
		return
	}
	var err error
	watcher, err = fsnotify.NewWatcher()
	if err != nil {
		logError("Не вдалося створити watcher: %v", err)
		return
	}
	watchDir := filepath.Dir(cfgFile)
	if _, err := os.Stat(watchDir); os.IsNotExist(err) {
		logError("Директорія '%s' не існує.", watchDir)
		watcher.Close()
		watcher = nil
		return
	}
	err = watcher.Add(watchDir)
	if err != nil {
		logError("Не вдалося додати '%s' до watcher: %v", watchDir, err)
		watcher.Close()
		watcher = nil
		return
	}
	logInfo("Моніторинг: %s (зміни у %s)", watchDir, filepath.Base(cfgFile))
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
						logDebug("Зміна %s. Перезапуск debounce.", currentCfgFile)
						debounceTimer.Reset(debounceDuration)
					}
				} else if event.Has(fsnotify.Rename) || event.Has(fsnotify.Remove) {
					if filepath.Dir(event.Name) == watchDir {
						logDebug("Зміна (rename/remove) у %s. Перезапуск debounce.", watchDir)
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
				logInfo("Debounce timer. Оновлення стану.")
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
		logInfo("Watcher зупинено.")
	}
}

func initializeLoadingRules() {
	stateMu.Lock()
	defer stateMu.Unlock()
	loadingRules = *clientcmd.NewDefaultClientConfigLoadingRules()
	kubeconfigFile = loadingRules.GetDefaultFilename()
	if os.Getenv(clientcmd.RecommendedConfigPathEnvVar) != "" {
		logDebug("Використовується %s", clientcmd.RecommendedConfigPathEnvVar)
		isKubeconfigEnvSet = true
	} else {
		logDebug("%s не встановлено, стандартний шлях: %s", clientcmd.RecommendedConfigPathEnvVar, kubeconfigFile)
		isKubeconfigEnvSet = false
	}
	logInfo("Ефективний шлях kubeconfig: %s (KUBECONFIG: %v)", kubeconfigFile, isKubeconfigEnvSet)
}

func openKubeFolder() {
	stateMu.RLock()
	cfgFile := kubeconfigFile
	stateMu.RUnlock()
	dir := filepath.Dir(cfgFile)
	if dir == "." || dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			logError("Не вдалося отримати home dir")
			return
		}
		dir = filepath.Join(home, ".kube")
	}
	logDebug("Спроба відкрити: %s", dir)
	var cmd *exec.Cmd
	switch goos := runtime.GOOS; goos {
	case "linux":
		cmd = exec.Command("xdg-open", dir)
	case "windows":
		cmd = exec.Command("explorer", dir)
	case "darwin":
		cmd = exec.Command("open", dir)
	default:
		logError("Непідтримувана ОС: %s", goos)
		return
	}
	err := cmd.Start()
	if err != nil {
		logError("Не вдалося відкрити '%s': %v", dir, err)
	}
}

func parseEmbeddedFont() error {
	if len(embeddedFontData) == 0 {
		logError("Критично: Дані вбудованого шрифту порожні! Переконайтесь, що файл шрифту існує і директива embed правильна.")
		return errors.New("дані вбудованого шрифту порожні")
	}
	var err error
	parsedFont, err = opentype.Parse(embeddedFontData)
	if err != nil {
		logError("Критично: Не вдалося розпарсити вбудований шрифт: %v", err)
		return fmt.Errorf("помилка парсингу шрифту: %w", err)
	}
	logInfo("Вбудований шрифт успішно розпарсено.")
	return nil
}

func onReady() {
	logInfo("Systray готовий. Версія Go: %s", runtime.Version())
	systray.SetTitle(labelLoading)
	systray.SetTooltip(labelLoading)

	if err := parseEmbeddedFont(); err != nil {
		systray.SetTitle(labelError)
		systray.SetTooltip(fmt.Sprintf("Помилка шрифту: %v", err))
		systray.AddMenuItem("Помилка завантаження шрифту!", err.Error())
		mQuit := systray.AddMenuItem("Вийти", "Exit")
		go func() { <-mQuit.ClickedCh; systray.Quit() }()
		return
	}

	loadingIconBytes, _ := generateIcon("...")
	if loadingIconBytes != nil {
		systray.SetIcon(loadingIconBytes)
	}

	initializeLoadingRules()
	menuItemContexts = make(map[*systray.MenuItem]string)

	_, checkPath, checkErr := loadKubeConfig()
	if checkErr != nil {
		logWarning("Початкова перевірка kubeconfig: %v", checkErr)
		if errors.Is(checkErr, os.ErrNotExist) || strings.Contains(checkErr.Error(), "не знайдено") {
			errorIconBytes, _ := generateIcon("?")
			if errorIconBytes != nil {
				systray.SetIcon(errorIconBytes)
			}
			systray.SetTitle(labelNoContext)
			systray.SetTooltip(fmt.Sprintf("Файл не знайдено:\n%s", checkPath))
		} else {
			errorIconBytes, _ := generateIcon("ERR")
			if errorIconBytes != nil {
				systray.SetIcon(errorIconBytes)
			}
			systray.SetTitle(labelError)
			systray.SetTooltip(fmt.Sprintf("Помилка конфігу:\n%v", checkErr))
		}
		systray.AddMenuItem(fmt.Sprintf("Помилка: %v", checkErr), "Помилка kubeconfig")
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
					logDebug("Клік на пункт без контексту?")
				}
			}
			logDebug("Горутина обробника кліків завершується.")
		}(item)
	}
	systray.AddSeparator()
	mRefresh := systray.AddMenuItem("Оновити", "Перезавантажити список та поточний контекст")
	mOpenFolder := systray.AddMenuItem("Відкрити папку конфігурації", "Відкрити папку, де лежить активний kubeconfig")
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
	logInfo("Запуск KubeContextSwitcher(Go-ClientGo-DynIcon)...")
	logInfo("Версія Go: %s", runtime.Version())
	systray.Run(onReady, onExit)
	logInfo("KubeContextSwitcher(Go-ClientGo-DynIcon) завершено.")
}
