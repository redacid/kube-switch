package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort" // Додано для сортування контекстів
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/getlantern/systray"
	// Залежності Kubernetes client-go
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	// Можливо знадобиться для деяких типів, якщо розширювати функціонал
	// _ "k8s.io/client-go/plugin/pkg/client/auth" // для cloud provider auth
)

const enableDebugLogging = true
const logPrefix = "KubeContextSwitcher(Go-ClientGo)"
const maxContextItems = 30
const maxTooltipLength = 120

// Константи іконок та міток залишаються ті ж самі
const iconLoading = "icons/loading.png"
const iconDefault = "icons/default.png"
const iconError = "icons/error.png"
const iconMain = "icons/icon.png"

const labelLoading = "Завантаження..."
const labelError = "Помилка"
const labelNoContext = "Немає контексту"

var arnRegex = regexp.MustCompile(`^arn:aws:eks:[^:]+:(\d+):cluster\/(.+)$`)

var (
	// Стан програми
	currentContext     string
	kubeconfigFile     string // Тепер це шлях, визначений clientcmd
	isKubeconfigEnvSet bool   // Визначаємо, чи встановлено змінну KUBECONFIG
	loadingRules       clientcmd.ClientConfigLoadingRules
	stateMu            sync.RWMutex

	// Стан UI (меню)
	currentContextItem *systray.MenuItem
	contextMenuItems   []*systray.MenuItem
	menuItemContexts   map[*systray.MenuItem]string
	menuMu             sync.Mutex

	// Моніторинг файлу
	watcher     *fsnotify.Watcher
	watcherDone chan bool
)

// --- Логування ---
func logDebug(format string, v ...interface{}) {
	if enableDebugLogging {
		log.Printf(logPrefix+" [DEBUG]: "+format, v...)
	}
}
func logInfo(format string, v ...interface{})    { log.Printf(logPrefix+" [INFO]: "+format, v...) }
func logWarning(format string, v ...interface{}) { log.Printf(logPrefix+" [WARN]: "+format, v...) }
func logError(format string, v ...interface{})   { log.Printf(logPrefix+" [ERROR]: "+format, v...) }

// --- Робота з конфігурацією через clientcmd ---

// Завантажує повну конфігурацію kubeconfig
func loadKubeConfig() (*api.Config, string, error) {
	stateMu.RLock()
	rules := loadingRules // Використовуємо глобально визначені правила
	stateMu.RUnlock()

	configPath := rules.GetDefaultFilename()
	logDebug("Спроба завантаження конфігурації з: %s", configPath)

	config, err := clientcmd.LoadFromFile(configPath)
	if err != nil {
		logError("Не вдалося завантажити kubeconfig з '%s': %v", configPath, err)
		// Перевіряємо, чи помилка пов'язана з відсутністю файлу
		if os.IsNotExist(err) {
			return nil, configPath, fmt.Errorf("файл конфігурації не знайдено: %s", configPath)
		}
		return nil, configPath, fmt.Errorf("помилка завантаження '%s': %w", configPath, err)
	}

	logDebug("Kubeconfig '%s' успішно завантажено.", configPath)
	return config, configPath, nil
}

// Отримує поточний контекст з конфігурації
func getCurrentContext() (string, error) {
	logDebug("Отримання поточного контексту через clientcmd...")
	config, _, err := loadKubeConfig()
	if err != nil {
		// Якщо конфіг не завантажено, контексту немає (або є помилка)
		return "", err // Повертаємо помилку завантаження
	}

	if config.CurrentContext == "" {
		logDebug("Поле CurrentContext у конфігурації порожнє.")
		return "", nil // Не помилка, просто не встановлено
	}

	// Перевіряємо, чи існує такий контекст у списку
	if _, exists := config.Contexts[config.CurrentContext]; !exists {
		logWarning("Поточний контекст '%s' вказано, але його немає у списку контекстів!", config.CurrentContext)
		// Можна повернути помилку або вважати, що контексту немає
		return "", fmt.Errorf("поточний контекст '%s' не знайдено у конфігурації", config.CurrentContext)
	}

	logDebug("Поточний контекст з конфігурації: %s", config.CurrentContext)
	return config.CurrentContext, nil
}

// Отримує список імен усіх контекстів з конфігурації
func getContexts() ([]string, error) {
	logDebug("Отримання списку контекстів через clientcmd...")
	config, _, err := loadKubeConfig()
	if err != nil {
		return nil, err // Повертаємо помилку завантаження
	}

	if len(config.Contexts) == 0 {
		logInfo("У конфігурації не знайдено жодного контексту.")
		return []string{}, nil
	}

	contexts := make([]string, 0, len(config.Contexts))
	for name := range config.Contexts {
		contexts = append(contexts, name)
	}

	// Сортуємо для стабільного порядку в меню
	sort.Strings(contexts)

	logDebug("Знайдено та відсортовано контекстів: %v", contexts)
	return contexts, nil
}

// Перемикає поточний контекст у файлі конфігурації
func switchContext(contextName string) error {
	logDebug("Перемикання на контекст '%s' через clientcmd...", contextName)

	config, configPath, err := loadKubeConfig() // Завантажуємо поточну конфігурацію
	if err != nil {
		logError("Не вдалося завантажити конфігурацію для перемикання контексту: %v", err)
		return fmt.Errorf("неможливо завантажити конфіг для зміни: %w", err)
	}

	// Перевіряємо, чи існує контекст, на який перемикаємось
	if _, exists := config.Contexts[contextName]; !exists {
		logError("Спроба перемкнутись на неіснуючий контекст: %s", contextName)
		return fmt.Errorf("контекст '%s' не знайдено у конфігурації", contextName)
	}

	// Перевіряємо, чи справді потрібно щось змінювати
	if config.CurrentContext == contextName {
		logInfo("Контекст '%s' вже є поточним. Перемикання не потрібне.", contextName)
		return nil // Нічого не робимо
	}

	// Змінюємо поточний контекст у завантаженій структурі
	config.CurrentContext = contextName
	logDebug("Встановлено CurrentContext = '%s' у структурі.", contextName)

	// Записуємо змінену конфігурацію назад у файл
	// Використовуємо WriteToFile для простоти. Вона перезаписує файл.
	// Потрібно бути обережним з правами доступу та можливими race conditions.
	logDebug("Спроба зберегти змінену конфігурацію у файл: %s", configPath)
	err = clientcmd.WriteToFile(*config, configPath)
	if err != nil {
		logError("Не вдалося записати змінену конфігурацію у файл '%s': %v", configPath, err)
		return fmt.Errorf("помилка збереження конфігурації: %w", err)
	}

	logInfo("Успішно перемкнено поточний контекст на '%s' у файлі %s", contextName, configPath)
	return nil
}

// --- Логіка відображення ---
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

// --- Оновлення UI (Systray) ---
// (Логіка Hide/Show залишається, але джерело даних змінилося)
func updateSystrayUI() {
	logDebug("Оновлення UI systray (Client-Go)...")

	stateMu.RLock()
	ctx := currentContext
	cfgFile := kubeconfigFile // Використовуємо шлях, визначений clientcmd
	cfgEnvSet := isKubeconfigEnvSet
	stateMu.RUnlock()

	displayName := getDisplayName(ctx)
	systray.SetTitle(displayName)
	setIcon(iconMain)

	// --- Встановлення Tooltip ---
	tooltip := labelNoContext
	var configPathDesc string
	if cfgEnvSet {
		configPathDesc = fmt.Sprintf("Конфіг (KUBECONFIG):\n%s\n(Авто-оновлення ВИМК.)", cfgFile)
	} else {
		// Якщо KUBECONFIG не встановлено, cfgFile буде стандартним шляхом
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

	// --- Оновлення пункту для поточного контексту ---
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

	// --- Оновлення списку доступних контекстів ---
	allContexts, err := getContexts() // Тепер використовує clientcmd
	if err != nil {
		// Показуємо помилку завантаження конфігурації
		logError("Не вдалося отримати контексти для меню (client-go): %v", err)
		// Приховуємо всі пункти контекстів та очищуємо мапу
		menuMu.Lock()
		for _, item := range contextMenuItems {
			item.Hide()
			delete(menuItemContexts, item)
		}
		menuMu.Unlock()
		// Можна додати пункт меню з помилкою
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
	logDebug("UI Systray оновлено (Client-Go)")
}

// Безпечно оновлює стан і викликає оновлення UI
func refreshState() {
	logDebug("Оновлення стану (client-go)...")
	setIcon(iconLoading)
	systray.SetTitle(labelLoading)
	systray.SetTooltip("Оновлення контексту...")

	// Отримуємо новий контекст, використовуючи clientcmd
	newContext, err := getCurrentContext() // Викликає loadKubeConfig всередині

	stateMu.Lock() // Блокуємо стан для запису
	refreshNeeded := false

	if err != nil {
		// Обробляємо помилки завантаження/парсингу конфігу
		logError("Помилка оновлення поточного контексту (client-go): %v", err)
		// Вважаємо, що контекст невідомий
		if currentContext != "" {
			logInfo("Контекст скинуто через помилку оновлення.")
			currentContext = ""
			refreshNeeded = true
		}
		// Зберігаємо помилку для показу? Поки що просто скидаємо контекст.
		stateMu.Unlock()
		setIcon(iconError)
		systray.SetTitle(labelError)
		systray.SetTooltip(fmt.Sprintf("Помилка оновлення: %v", err)) // Показуємо помилку в tooltip
		updateSystrayUI()                                             // Оновити меню (покаже помилку/порожній список)
		return
	}

	// Перевіряємо, чи контекст справді змінився
	if newContext != currentContext {
		logInfo("Контекст змінився з '%s' на '%s'", currentContext, newContext)
		currentContext = newContext
		refreshNeeded = true
	} else {
		logDebug("Контекст не змінився ('%s')", currentContext)
		// Все одно оновлюємо UI, бо список контекстів міг змінитися
		refreshNeeded = true
	}
	stateMu.Unlock()

	if refreshNeeded {
		updateSystrayUI()
	}
}

// Обробляє клік на пункті меню контексту
func handleContextSwitchClick(contextName string) {
	logDebug("Обробка кліку для перемикання на '%s' (client-go)", contextName)
	setIcon(iconLoading)
	systray.SetTooltip("Перемикання контексту...")

	err := switchContext(contextName) // Тепер використовує clientcmd
	if err != nil {
		logError("Помилка перемикання контексту (client-go) на '%s': %v", contextName, err)
		// Показуємо помилку користувачу
		systray.SetTooltip(fmt.Sprintf("Помилка перемикання: %v", err))
		// Можна додати сповіщення
		time.Sleep(3 * time.Second) // Затримка, щоб побачити tooltip
	}
	// Завжди оновлюємо стан та UI після спроби перемикання
	refreshState()
}

// --- Моніторинг файлу ---
// (Логіка залишається та сама, але kubeconfigFile визначається інакше)
func setupFileWatcher() {
	stateMu.RLock()
	cfgFile := kubeconfigFile // Використовуємо шлях, визначений loadingRules
	cfgEnvSet := isKubeconfigEnvSet
	stateMu.RUnlock()

	if cfgEnvSet {
		logInfo("Встановлено KUBECONFIG. Моніторинг файлу вимкнено.")
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
				stateMu.RUnlock() // Перевіряємо актуальний шлях
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
				logInfo("Debounce timer. Оновлення стану через зміну файлу.")
				refreshState()
			case <-watcherDone:
				logInfo("Зупинка file watcher.")
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

// --- Допоміжні функції ---

// Визначає правила завантаження та ефективний шлях до kubeconfig
func initializeLoadingRules() {
	stateMu.Lock()
	defer stateMu.Unlock()

	loadingRules = *clientcmd.NewDefaultClientConfigLoadingRules()
	kubeconfigFile = loadingRules.GetDefaultFilename() // Отримуємо ефективний шлях

	// Перевіряємо, чи використовується змінна KUBECONFIG
	if os.Getenv(clientcmd.RecommendedConfigPathEnvVar) != "" {
		logDebug("Використовується змінна середовища %s", clientcmd.RecommendedConfigPathEnvVar)
		isKubeconfigEnvSet = true
	} else {
		logDebug("Змінна %s не встановлена, використовується стандартний шлях: %s", clientcmd.RecommendedConfigPathEnvVar, kubeconfigFile)
		isKubeconfigEnvSet = false
	}
	logInfo("Ефективний шлях kubeconfig: %s (KUBECONFIG встановлено: %v)", kubeconfigFile, isKubeconfigEnvSet)
}

// Видалено checkKubectlExists

func loadIcon(path string) []byte {
	logDebug("Завантаження іконки: %s", path)
	data, err := ioutil.ReadFile(path)
	if err != nil {
		logError("Не вдалося завантажити '%s': %v", path, err)
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
			logError("Не вдалося отримати home dir")
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
		logError("Непідтримувана ОС: %s", goos)
		return
	}
	err := cmd.Start()
	if err != nil {
		logError("Не вдалося відкрити '%s': %v", dir, err)
	}
}

// --- Головна логіка програми (Systray) ---

func onReady() {
	logInfo("Systray готовий. Версія Go: %s", runtime.Version())
	systray.SetTitle(labelLoading)
	systray.SetTooltip(labelLoading)
	setIcon(iconLoading)

	// Ініціалізуємо правила завантаження конфігурації
	initializeLoadingRules()
	// Ініціалізація мапи для меню
	menuItemContexts = make(map[*systray.MenuItem]string)

	// Перевірка: чи можемо ми взагалі завантажити конфіг?
	_, checkPath, checkErr := loadKubeConfig()
	if checkErr != nil {
		// Не фатальна помилка, якщо файл просто не існує, але покажемо це
		logWarning("Початкова перевірка kubeconfig не вдалася: %v", checkErr)
		if errors.Is(checkErr, os.ErrNotExist) || strings.Contains(checkErr.Error(), "файл конфігурації не знайдено") {
			systray.SetTitle(labelNoContext)
			systray.SetTooltip(fmt.Sprintf("Файл конфігурації не знайдено:\n%s", checkPath))
			setIcon(iconError) // Можна використати іконку попередження
		} else {
			systray.SetTitle(labelError)
			systray.SetTooltip(fmt.Sprintf("Помилка завантаження конфігурації:\n%v", checkErr))
			setIcon(iconError)
		}
		// Додаємо мінімальне меню для виходу
		systray.AddMenuItem(fmt.Sprintf("Помилка: %v", checkErr), "Помилка завантаження kubeconfig")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Вийти", "Exit")
		go func() { <-mQuit.ClickedCh; systray.Quit() }()
		return // Не продовжуємо створення повного меню
	}

	// --- Створення пунктів меню ---
	currentContextItem = systray.AddMenuItem(labelLoading, "Поточний контекст Kubernetes")
	systray.AddSeparator()
	contextMenuItems = make([]*systray.MenuItem, 0, maxContextItems)
	for i := 0; i < maxContextItems; i++ {
		item := systray.AddMenuItem(fmt.Sprintf("placeholder_%d", i), "")
		item.Hide()
		contextMenuItems = append(contextMenuItems, item)
		go func(menuItem *systray.MenuItem) { // Горутина обробника кліків
			for range menuItem.ClickedCh {
				menuMu.Lock()
				contextName, ok := menuItemContexts[menuItem]
				menuMu.Unlock()
				if ok && contextName != "" {
					handleContextSwitchClick(contextName)
				} else {
					logDebug("Клік на пункт меню без контексту?")
				}
			}
			logDebug("Горутина обробника кліків завершується.")
		}(item)
	}
	systray.AddSeparator()
	mRefresh := systray.AddMenuItem("Оновити", "Перезавантажити список та поточний контекст")
	mOpenFolder := systray.AddMenuItem("Відкрити папку конфігурації", "Відкрити папку, де лежить активний kubeconfig") // Змінено текст
	mQuit := systray.AddMenuItem("Вийти", "Завершити програму")
	go func() { // Горутина для статичних пунктів
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

	refreshState()     // Початкове оновлення стану та UI
	setupFileWatcher() // Запуск моніторингу файлу
}

func onExit() {
	logInfo("Вихід з systray...")
	stopFileWatcher()
	logInfo("Очищення завершено.")
}

// --- Точка входу ---
func main() {
	log.SetFlags(log.Ldate | log.Ltime)
	logInfo("Запуск KubeContextSwitcher(Go-ClientGo)...")
	logInfo("Версія Go: %s", runtime.Version())
	systray.Run(onReady, onExit) // Запуск systray (блокуючий)
	logInfo("KubeContextSwitcher(Go-ClientGo) завершено.")
}
