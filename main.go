package main

import (
	"context"
	// "embed"
	// "errors"
	"fmt"
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

	// "github.com/fsnotify/fsnotify"

	// Fyne GUI Toolkit
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"

	// Kubernetes client-go & API types
	corev1 "k8s.io/api/core/v1" // Додано для типу Node
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	// _ "k8s.io/client-go/plugin/pkg/client/auth"
)

// //go:embed icon.png
// var iconData []byte

const enableDebugLogging = true
const logPrefix = "GoKubeLens(Step4-Details)" // Оновлено префікс
const maxContextItems = 50

const labelLoading = "Завантаження..."
const labelError = "Помилка"
const labelNoContext = "Немає контексту"
const labelBackToList = "<- Назад до списку Вузлів" // Змінено текст кнопки
const appTitle = "Go Kube Manager (Lens Clone) - Step 4"

var arnRegex = regexp.MustCompile(`^arn:aws:eks:[^:]+:(\d+):cluster\/(.+)$`)

var (
	fyneApp    fyne.App
	mainWindow fyne.Window
	// UI елементи
	currentContextLabel *widget.Label
	contextListWidget   *widget.List
	resourceListWidget  *widget.List    // Список ресурсів (вузлів)
	resourceDetailArea  *fyne.Container // Контейнер для деталей (форма + кнопка Назад)
	rightPanelContainer *fyne.Container // Контейнер для перемикання (список/деталі) - тип *fyne.Container
	statusBar           *widget.Label
	desktopApp          desktop.App
	trayMenu            *fyne.Menu

	// Стан програми
	currentContextName   string
	connectedContextName string
	allContextNames      []string
	currentNodes         []corev1.Node // Зберігаємо повні об'єкти вузлів
	kubeconfigFile       string
	isKubeconfigEnvSet   bool
	loadingRules         clientcmd.ClientConfigLoadingRules
	currentClientset     *kubernetes.Clientset
	stateMu              sync.RWMutex
)

func logDebug(format string, v ...interface{}) {
	if enableDebugLogging {
		log.Printf(logPrefix+" [DEBUG]: "+format, v...)
	}
}
func logInfo(format string, v ...interface{})    { log.Printf(logPrefix+" [INFO]: "+format, v...) }
func logWarning(format string, v ...interface{}) { log.Printf(logPrefix+" [WARN]: "+format, v...) }
func logError(format string, v ...interface{})   { log.Printf(logPrefix+" [ERROR]: "+format, v...) }

// --- Робота з Kubeconfig (clientcmd) ---
func loadKubeConfig() (*api.Config, string, error) {
	stateMu.RLock()
	rules := loadingRules
	stateMu.RUnlock()
	configPath := rules.GetDefaultFilename()
	logDebug("Завантаження конфігу: %s", configPath)
	config, err := clientcmd.LoadFromFile(configPath)
	if err != nil {
		logError("Не вдалося завантажити '%s': %v", configPath, err)
		if os.IsNotExist(err) {
			return nil, configPath, fmt.Errorf("файл не знайдено: %s", configPath)
		}
		return nil, configPath, fmt.Errorf("помилка '%s': %w", configPath, err)
	}
	logDebug("'%s' завантажено.", configPath)
	return config, configPath, nil
}
func getCurrentContextFromFile() (string, error) {
	logDebug("Отримання поточного контексту з файлу...")
	config, _, err := loadKubeConfig()
	if err != nil {
		return "", err
	}
	if config.CurrentContext == "" {
		logDebug("CurrentContext порожній.")
		return "", nil
	}
	if _, exists := config.Contexts[config.CurrentContext]; !exists {
		logWarning("Поточний '%s' не знайдено у списку!", config.CurrentContext)
		return "", fmt.Errorf("контекст '%s' не знайдено", config.CurrentContext)
	}
	logDebug("Поточний з файлу: %s", config.CurrentContext)
	return config.CurrentContext, nil
}
func getContexts() ([]string, error) {
	logDebug("Отримання списку контекстів...")
	config, _, err := loadKubeConfig()
	if err != nil {
		return nil, err
	}
	if len(config.Contexts) == 0 {
		logInfo("Контексти не знайдено.")
		return []string{}, nil
	}
	contexts := make([]string, 0, len(config.Contexts))
	for name := range config.Contexts {
		contexts = append(contexts, name)
	}
	sort.Strings(contexts)
	logDebug("Знайдено: %v", contexts)
	return contexts, nil
}
func switchContext(contextName string) error {
	logDebug("Перемикання на '%s'...", contextName)
	config, configPath, err := loadKubeConfig()
	if err != nil {
		logError("Не вдалося завантажити конфіг: %v", err)
		return fmt.Errorf("помилка завантаження: %w", err)
	}
	if _, exists := config.Contexts[contextName]; !exists {
		logError("Неіснуючий контекст: %s", contextName)
		return fmt.Errorf("контекст '%s' не знайдено", contextName)
	}
	if config.CurrentContext == contextName {
		logInfo("Вже на '%s'.", contextName)
		return nil
	}
	config.CurrentContext = contextName
	logDebug("Встановлено '%s'", contextName)
	logDebug("Збереження у %s", configPath)
	err = clientcmd.WriteToFile(*config, configPath)
	if err != nil {
		logError("Не вдалося записати '%s': %v", configPath, err)
		return fmt.Errorf("помилка збереження: %w", err)
	}
	logInfo("Перемкнено на '%s' у %s", contextName, configPath)
	return nil
}

// --- Підключення до кластера ---
func connectToCluster(contextName string) (*kubernetes.Clientset, string, error) {
	logInfo("Спроба підключення до: %s", contextName)
	if statusBar != nil {
		statusBar.SetText(fmt.Sprintf("Підключення до '%s'...", getDisplayName(contextName)))
	}
	configOverrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	stateMu.RLock()
	rules := loadingRules
	stateMu.RUnlock()
	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(&rules, configOverrides)
	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		logError("Помилка rest.Config '%s': %v", contextName, err)
		if statusBar != nil {
			statusBar.SetText(fmt.Sprintf("Помилка конфігу '%s': %v", getDisplayName(contextName), err))
		}
		return nil, "", fmt.Errorf("помилка конфігу %s: %w", contextName, err)
	}
	restConfig.Timeout = 10 * time.Second
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		logError("Помилка clientset '%s': %v", contextName, err)
		if statusBar != nil {
			statusBar.SetText(fmt.Sprintf("Помилка клієнта '%s': %v", getDisplayName(contextName), err))
		}
		return nil, "", fmt.Errorf("помилка клієнта %s: %w", contextName, err)
	}
	logDebug("Clientset '%s' створено.", contextName)
	serverVersion, err := clientset.Discovery().ServerVersion()
	if err != nil {
		logError("Помилка версії '%s': %v", contextName, err)
		if statusBar != nil {
			statusBar.SetText(fmt.Sprintf("Помилка версії '%s': %v", getDisplayName(contextName), err))
		}
		return clientset, "Помилка версії", fmt.Errorf("помилка версії %s: %w", contextName, err)
	}
	versionString := serverVersion.GitVersion
	logInfo("Успіх '%s'. Версія: %s", contextName, versionString)
	if statusBar != nil {
		statusBar.SetText(fmt.Sprintf("Підключено: %s (Сервер: %s)", getDisplayName(contextName), versionString))
	}
	return clientset, versionString, nil
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
	return contextName
}

// --- Оновлення UI віджетів Fyne ---
func updateUIWidgets() {
	logDebug("Оновлення UI віджетів (Fyne)...")
	stateMu.RLock()
	ctxFromFile := currentContextName
	connCtx := connectedContextName
	ctxList := allContextNames
	nodes := currentNodes
	statusMsg := ""
	if statusBar != nil {
		statusMsg = statusBar.Text
	}
	stateMu.RUnlock()
	displayCtxFromFile := labelNoContext
	if ctxFromFile != "" {
		displayCtxFromFile = getDisplayName(ctxFromFile)
	}
	if currentContextLabel != nil {
		currentContextLabel.SetText("Поточний у файлі: " + displayCtxFromFile)
	}

	if contextListWidget != nil {
		contextListWidget.Refresh()
		targetSelection := connCtx
		if targetSelection == "" {
			targetSelection = ctxFromFile
		}
		selectedIndex := -1
		for i, name := range ctxList {
			if name == targetSelection {
				selectedIndex = i
				break
			}
		}
		if selectedIndex != -1 {
			contextListWidget.Select(selectedIndex)
		} else {
			contextListWidget.UnselectAll()
		}
	}

	if resourceListWidget != nil {
		logDebug("Оновлення списку ресурсів у UI (%d вузлів)", len(nodes))
		resourceListWidget.Refresh()
	}
	// Оновлення деталей НЕ ПОТРІБНЕ тут, воно відбувається при виклику displayNodeDetails

	// updateSystemTrayMenu() // Трей вимкнено

	if statusBar != nil && !strings.HasPrefix(statusMsg, "Помилка") && !strings.HasPrefix(statusMsg, "Підключення") {
		statusBar.SetText(fmt.Sprintf("Контекстів: %d | Вузлів: %d", len(ctxList), len(nodes)))
	}
	logDebug("Оновлення UI віджетів завершено.")
}

// --- Завантаження даних, оновлення стану та ВИКЛИК оновлення UI ---
func loadAndUpdateState() {
	logInfo("Завантаження конфігурації та оновлення стану...")
	if statusBar != nil {
		statusBar.SetText(labelLoading + "...")
	}
	ctxFromFile, errCtxFile := getCurrentContextFromFile()
	if errCtxFile != nil {
		logError("Не вдалося отримати поточний контекст: %v", errCtxFile)
		ctxFromFile = ""
	}
	ctxList, errCtxList := getContexts()
	if errCtxList != nil {
		logError("Не вдалося отримати список контекстів: %v", errCtxList)
		ctxList = []string{}
	}

	stateMu.Lock()
	currentContextName = ctxFromFile
	allContextNames = ctxList
	connectedContextName = ""
	currentClientset = nil
	currentNodes = []corev1.Node{}
	stateMu.Unlock()

	var statusMsg string
	if errCtxFile != nil && errCtxList != nil {
		statusMsg = fmt.Sprintf("Помилка конт. та списку!")
	} else if errCtxList != nil {
		statusMsg = fmt.Sprintf("Помилка списку: %v", errCtxList)
	} else if errCtxFile != nil {
		statusMsg = fmt.Sprintf("Помилка поточного: %v", errCtxFile)
	} else {
		statusMsg = fmt.Sprintf("Контекстів: %d", len(ctxList))
	}
	if statusBar != nil {
		statusBar.SetText(statusMsg)
	}

	displayNodeList() // Показуємо список ресурсів (порожній)

	logDebug("Виклик оновлення UI віджетів після завантаження")
	updateUIWidgets()
	logInfo("Завантаження та оновлення стану завершено.")
}

// --- Завантаження ресурсів та оновлення UI ---
func loadResourcesAndUpdateUI(clientset *kubernetes.Clientset, contextName string) {
	if clientset == nil {
		logWarning("Спроба завантажити ресурси без clientset.")
		displayNodeList()
		updateUIWidgets()
		return
	} // Додано показ списку та оновлення UI
	logInfo("Завантаження ресурсів для контексту: %s", contextName)
	if statusBar != nil {
		statusBar.SetText(fmt.Sprintf("Завантаження вузлів для '%s'...", getDisplayName(contextName)))
	}

	nodes, err := clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	loadedNodes := []corev1.Node{}
	statusMsg := ""

	if err != nil {
		logError("Не вдалося отримати список вузлів для '%s': %v", contextName, err)
		statusMsg = fmt.Sprintf("Помилка завантаження вузлів: %v", err)
	} else {
		logDebug("Отримано %d вузлів.", len(nodes.Items))
		loadedNodes = nodes.Items
		sort.Slice(loadedNodes, func(i, j int) bool { return loadedNodes[i].Name < loadedNodes[j].Name })
		statusMsg = fmt.Sprintf("Підключено: %s | Вузлів: %d", getDisplayName(contextName), len(loadedNodes))
	}

	stateMu.Lock()
	currentNodes = loadedNodes
	stateMu.Unlock()
	if statusBar != nil {
		statusBar.SetText(statusMsg)
	}

	displayNodeList() // Показуємо оновлений список вузлів
	updateUIWidgets()
	logInfo("Завантаження та оновлення ресурсів завершено.")
}

// Обгортка для підключення, завантаження ресурсів та оновлення UI
func connectLoadAndRefresh(ctxName string) {
	displayNodeList() // Одразу показуємо (можливо порожній) список вузлів
	if resourceListWidget != nil {
		resourceListWidget.Refresh()
	} // Оновлюємо його

	clientset, _, err := connectToCluster(ctxName)

	stateMu.Lock()
	if err == nil {
		connectedContextName = ctxName
		currentClientset = clientset
		currentNodes = []corev1.Node{}
		logDebug("Збережено clientset: %s", ctxName)
		stateMu.Unlock()
		go loadResourcesAndUpdateUI(clientset, ctxName)
	} else {
		connectedContextName = ""
		currentClientset = nil
		currentNodes = []corev1.Node{}
		logDebug("Помилка підключення.")
		stateMu.Unlock()
		displayNodeList()
		updateUIWidgets()
	}
}

// --- Функції для перемикання вмісту правої панелі ---

// Показує список вузлів у правій панелі
func displayNodeList() {
	logDebug("Показ списку вузлів")
	if rightPanelContainer != nil && resourceListWidget != nil {
		// Оновлюємо дані списку перед тим як його показати
		resourceListWidget.Refresh()
		if len(rightPanelContainer.Objects) == 0 || rightPanelContainer.Objects[0] != resourceListWidget {
			rightPanelContainer.Objects = []fyne.CanvasObject{resourceListWidget}
			rightPanelContainer.Refresh()
		}
	} else {
		logError("rightPanelContainer або resourceListWidget є nil при показі списку")
	}
}

// Показує деталі вузла у правій панелі
func displayNodeDetails(node corev1.Node) {
	logDebug("Показ деталей для вузла: %s", node.Name)
	if rightPanelContainer != nil {
		// Створюємо віджети для деталей
		form := widget.NewForm()
		form.Append("Name", widget.NewLabel(node.Name))
		form.Append("Created", widget.NewLabel(node.CreationTimestamp.Format(time.RFC1123)))

		statusItems := []string{}
		for _, cond := range node.Status.Conditions {
			if cond.Status == corev1.ConditionTrue {
				statusItems = append(statusItems, string(cond.Type))
			}
		}
		if len(statusItems) == 0 {
			statusItems = append(statusItems, "Unknown")
		}
		form.Append("Status", widget.NewLabel(strings.Join(statusItems, ", ")))

		roles := []string{}
		for label := range node.Labels {
			if strings.HasPrefix(label, "node-role.kubernetes.io/") {
				roles = append(roles, strings.TrimPrefix(label, "node-role.kubernetes.io/"))
			}
		}
		if len(roles) == 0 {
			roles = append(roles, "<none>")
		}
		sort.Strings(roles)
		form.Append("Roles", widget.NewLabel(strings.Join(roles, ", ")))

		form.Append("Kubelet Version", widget.NewLabel(node.Status.NodeInfo.KubeletVersion))
		form.Append("OS Image", widget.NewLabel(node.Status.NodeInfo.OSImage))
		form.Append("Kernel Version", widget.NewLabel(node.Status.NodeInfo.KernelVersion))
		form.Append("Container Runtime", widget.NewLabel(node.Status.NodeInfo.ContainerRuntimeVersion))

		internalIP := ""
		externalIP := ""
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				internalIP = addr.Address
			}
			if addr.Type == corev1.NodeExternalIP {
				externalIP = addr.Address
			}
		}
		form.Append("Internal IP", widget.NewLabel(internalIP))
		form.Append("External IP", widget.NewLabel(externalIP))

		// --- Додамо ресурси (Capacity) ---
		cpu := node.Status.Capacity[corev1.ResourceCPU]
		mem := node.Status.Capacity[corev1.ResourceMemory]
		pods := node.Status.Capacity[corev1.ResourcePods]
		form.Append("CPU (Capacity)", widget.NewLabel(cpu.String()))
		form.Append("Memory (Capacity)", widget.NewLabel(mem.String()))
		form.Append("Pods (Capacity)", widget.NewLabel(pods.String()))

		backButton := widget.NewButton(labelBackToList, func() { displayNodeList() })

		// Створюємо контейнер для деталей з прокруткою
		scrollableForm := container.NewVScroll(form)
		detailView := container.NewBorder(backButton, nil, nil, nil, scrollableForm)

		// Встановлюємо новий вміст у праву панель
		rightPanelContainer.Objects = []fyne.CanvasObject{detailView}
		rightPanelContainer.Refresh()
	} else {
		logError("rightPanelContainer є nil при показі деталей")
	}
}

// --- Допоміжні функції ---
func initializeLoadingRules() {
	stateMu.Lock()
	defer stateMu.Unlock()
	loadingRules = *clientcmd.NewDefaultClientConfigLoadingRules()
	kubeconfigFile = loadingRules.GetDefaultFilename()
	if os.Getenv(clientcmd.RecommendedConfigPathEnvVar) != "" {
		isKubeconfigEnvSet = true
	} else {
		isKubeconfigEnvSet = false
	}
	logInfo("Шлях Kubeconfig: %s (KUBECONFIG: %v)", kubeconfigFile, isKubeconfigEnvSet)
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

// --- Створення меню Fyne ---
func buildContextMenu() *fyne.Menu {
	logDebug("Побудова меню Fyne...")
	stateMu.RLock()
	currentCtx := currentContextName
	connCtx := connectedContextName
	stateMu.RUnlock()
	refreshItem := fyne.NewMenuItem("Оновити список", func() { logDebug("Клік 'Оновити'"); go loadAndUpdateState() })
	openFolderItem := fyne.NewMenuItem("Відкрити папку конфігурації", func() { logDebug("Клік 'Відкрити папку'"); openKubeFolder() })
	quitItem := fyne.NewMenuItem("Вийти", func() { logInfo("Клік 'Вийти'"); fyneApp.Quit() })
	contextItems := []*fyne.MenuItem{}
	allContexts, err := getContexts()
	if err != nil {
		logError("Не вдалося отримати контексти для меню: %v", err)
		contextItems = append(contextItems, fyne.NewMenuItem(fmt.Sprintf("%s: %v", labelError, err), nil))
	} else {
		if len(allContexts) == 0 {
			contextItems = append(contextItems, fyne.NewMenuItem("(Немає контекстів)", nil))
		}
		for _, ctxName := range allContexts {
			name := ctxName
			label := getDisplayName(name)
			targetSelection := connCtx
			if targetSelection == "" {
				targetSelection = currentCtx
			}
			if name == targetSelection {
				label = "✓ " + label
			} else {
				label = "  " + label
			}
			var action func()
			if name != connCtx {
				action = func() { go connectLoadAndRefresh(name) }
			} else {
				action = nil
			}
			item := fyne.NewMenuItem(label, action)
			contextItems = append(contextItems, item)
		}
	}
	menu := fyne.NewMenu("Дії", contextItems...)
	menu.Items = append(menu.Items, fyne.NewMenuItemSeparator(), refreshItem, openFolderItem)
	menu.Items = append(menu.Items, fyne.NewMenuItemSeparator(), quitItem)
	return menu
}
func showWindowContextMenu(pos fyne.Position) {
	menu := buildContextMenu()
	if mainWindow != nil {
		widget.ShowPopUpMenuAtPosition(menu, mainWindow.Canvas(), pos)
	}
}

// --- Допоміжний тип для обробки правого кліку на контейнері ---
type tappableContainer struct {
	widget.BaseWidget
	content fyne.CanvasObject
}

func (t *tappableContainer) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(t.content)
}
func (t *tappableContainer) TappedSecondary(ev *fyne.PointEvent) {
	logDebug("Правий клік на контейнері")
	showWindowContextMenu(ev.AbsolutePosition)
}
func (t *tappableContainer) MinSize() fyne.Size { return t.content.MinSize() }

// --- Головна функція та запуск Fyne ---
func main() {
	log.SetFlags(log.Ldate | log.Ltime)
	logInfo("Запуск " + logPrefix + "...")
	logInfo("Версія Go: %s", runtime.Version())

	initializeLoadingRules()
	fyneApp = app.New()
	mainWindow = fyneApp.NewWindow(appTitle)

	// --- Створюємо UI елементи ---
	currentContextLabel = widget.NewLabel(labelLoading)
	statusBar = widget.NewLabel("Ініціалізація...")
	// Список контекстів
	contextListWidget = widget.NewList(
		func() int { stateMu.RLock(); defer stateMu.RUnlock(); return len(allContextNames) },
		func() fyne.CanvasObject { return widget.NewLabel("template") },
		func(id widget.ListItemID, item fyne.CanvasObject) {
			stateMu.RLock()
			name := ""
			if id >= 0 && id < len(allContextNames) {
				name = allContextNames[id]
			}
			stateMu.RUnlock()
			item.(*widget.Label).SetText(getDisplayName(name))
		},
	)
	contextListWidget.OnSelected = func(id widget.ListItemID) {
		stateMu.RLock()
		selectedName := ""
		if id >= 0 && id < len(allContextNames) {
			selectedName = allContextNames[id]
		}
		stateMu.RUnlock()
		if selectedName != "" {
			logInfo("Вибрано контекст: %s", selectedName)
			stateMu.RLock()
			alreadyConnected := connectedContextName
			stateMu.RUnlock()
			if selectedName != alreadyConnected {
				go connectLoadAndRefresh(selectedName)
			}
		}
	}
	// Список ресурсів (вузлів)
	resourceListWidget = widget.NewList(
		func() int { stateMu.RLock(); defer stateMu.RUnlock(); return len(currentNodes) }, // Використовуємо currentNodes
		func() fyne.CanvasObject { return widget.NewLabel("template") },
		func(id widget.ListItemID, item fyne.CanvasObject) {
			stateMu.RLock()
			nodeName := ""
			if id >= 0 && id < len(currentNodes) {
				nodeName = currentNodes[id].Name
			}
			stateMu.RUnlock()
			item.(*widget.Label).SetText(nodeName)
		}, // Показуємо ім'я вузла
	)
	resourceListWidget.OnSelected = func(id widget.ListItemID) {
		stateMu.RLock()
		var selectedNode *corev1.Node
		if id >= 0 && id < len(currentNodes) {
			nodeCopy := currentNodes[id]
			selectedNode = &nodeCopy
		}
		stateMu.RUnlock() // Копіюємо, щоб уникнути гонки даних
		if selectedNode != nil {
			logInfo("Вибрано вузол: %s", selectedNode.Name)
			displayNodeDetails(*selectedNode)
		} else {
			logWarning("Спроба вибрати неіснуючий вузол, ID: %d", id)
		}
	}

	// --- Збираємо макет вікна ---
	leftPanel := container.NewBorder(container.NewPadded(widget.NewLabel("Контексти:")), nil, nil, nil, contextListWidget)
	// Права панель - це Max контейнер, що перемикається
	rightPanelContainer = container.NewMax(resourceListWidget) // Починаємо зі списку ресурсів
	tappableRightPanel := &tappableContainer{content: rightPanelContainer}
	tappableRightPanel.ExtendBaseWidget(tappableRightPanel)
	split := container.NewHSplit(leftPanel, tappableRightPanel)
	split.Offset = 0.3
	mainLayout := container.NewBorder(container.NewVBox(currentContextLabel, widget.NewSeparator()), statusBar, nil, nil, split)
	mainWindow.SetContent(mainLayout)

	mainWindow.Resize(fyne.NewSize(800, 600))
	mainWindow.CenterOnScreen()
	mainWindow.SetCloseIntercept(func() { logInfo("Закриття вікна..."); fyneApp.Quit() })

	go loadAndUpdateState()
	mainWindow.ShowAndRun()
	logInfo(logPrefix + " завершено.")
}
