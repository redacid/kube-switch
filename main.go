package main

import (
	"context"
	"embed"
	//"errors"
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
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	// Kubernetes client-go & API types
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	// _ "k8s.io/client-go/plugin/pkg/client/auth"
)

//go:embed icon.png
var iconData []byte
var _ embed.FS

const enableDebugLogging = true
const logPrefix = "GoKubeLens(Step6-FixUndef)" // Оновлено префікс
const maxContextItems = 50

const labelLoading = "Завантаження..."
const labelError = "Помилка"
const labelNoContext = "Немає контексту"
const labelBackToList = "<- Назад до списку"
const appTitle = "Go Kube Manager (Lens Clone) - Step 6"

// Мапи для дерева ресурсів
type resourceTreeNodeID = string

var resourceTreeData = map[resourceTreeNodeID][]resourceTreeNodeID{"": {"Cluster", "Workloads", "Network", "Storage", "Configuration", "Access Control"}, "Cluster": {"Namespaces", "Nodes"}, "Workloads": {"Pods", "Deployments", "StatefulSets", "DaemonSets", "ReplicaSets", "Jobs", "CronJobs"}, "Network": {"Services", "Ingresses"}, "Storage": {"PersistentVolumes", "PersistentVolumeClaims", "StorageClasses"}, "Configuration": {"ConfigMaps", "Secrets"}, "Access Control": {"ServiceAccounts", "Roles", "RoleBindings", "ClusterRoles", "ClusterRoleBindings"}}
var resourceLeafNodes = map[resourceTreeNodeID]bool{"Namespaces": true, "Nodes": true, "Pods": true, "Deployments": true, "StatefulSets": true, "DaemonSets": true, "ReplicaSets": true, "Jobs": true, "CronJobs": true, "Services": true, "Ingresses": true, "PersistentVolumes": true, "PersistentVolumeClaims": true, "StorageClasses": true, "ConfigMaps": true, "Secrets": true, "ServiceAccounts": true, "Roles": true, "RoleBindings": true, "ClusterRoles": true, "ClusterRoleBindings": true}

var arnRegex = regexp.MustCompile(`^arn:aws:eks:[^:]+:(\d+):cluster\/(.+)$`)

var (
	fyneApp             fyne.App
	mainWindow          fyne.Window
	currentContextLabel *widget.Label
	contextListWidget   *widget.List
	resourceTypeTree    *widget.Tree
	resourceListWidget  *widget.List
	rightPanelContainer *fyne.Container
	statusBar           *widget.Label
	desktopApp          desktop.App
	trayMenu            *fyne.Menu

	currentContextName   string
	connectedContextName string
	allContextNames      []string
	selectedResourceType string
	currentNodes         []corev1.Node
	currentNamespaces    []corev1.Namespace
	currentPods          []corev1.Pod
	currentDeployments   []appsv1.Deployment
	currentStatefulSets  []appsv1.StatefulSet
	currentDaemonSets    []appsv1.DaemonSet
	currentReplicaSets   []appsv1.ReplicaSet
	currentJobs          []batchv1.Job
	currentCronJobs      []batchv1.CronJob

	kubeconfigFile     string
	isKubeconfigEnvSet bool
	loadingRules       clientcmd.ClientConfigLoadingRules
	currentClientset   *kubernetes.Clientset
	stateMu            sync.RWMutex
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
	resType := selectedResourceType
	statusMsg := ""
	if statusBar != nil {
		statusMsg = statusBar.Text
	}
	nodesCount := len(currentNodes)
	nsCount := len(currentNamespaces)
	podsCount := len(currentPods)
	deployCount := len(currentDeployments)
	stsCount := len(currentStatefulSets)
	dsCount := len(currentDaemonSets)
	rsCount := len(currentReplicaSets)
	jobCount := len(currentJobs)
	cronJobCount := len(currentCronJobs)
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
	if resourceTypeTree != nil {
		resourceTypeTree.Refresh()
		if resType != "" {
			resourceTypeTree.Select(resType)
		} else {
			resourceTypeTree.UnselectAll()
		}
	}
	resourceCount := 0
	if resourceListWidget != nil {
		stateMu.RLock()
		switch resType {
		case "Namespaces":
			resourceCount = nsCount
		case "Nodes":
			resourceCount = nodesCount
		case "Pods":
			resourceCount = podsCount
		case "Deployments":
			resourceCount = deployCount
		case "StatefulSets":
			resourceCount = stsCount
		case "DaemonSets":
			resourceCount = dsCount
		case "ReplicaSets":
			resourceCount = rsCount
		case "Jobs":
			resourceCount = jobCount
		case "CronJobs":
			resourceCount = cronJobCount
		default:
			resourceCount = 0
		}
		stateMu.RUnlock()
		logDebug("Оновлення списку ресурсів '%s' у UI (%d елементів)", resType, resourceCount)
		resourceListWidget.Refresh()
	}
	//updateSystemTrayMenu()
	if statusBar != nil && !strings.HasPrefix(statusMsg, "Помилка") && !strings.HasPrefix(statusMsg, "Підключення") && !strings.HasPrefix(statusMsg, "Завантаження") {
		statusBar.SetText(fmt.Sprintf("Контекстів: %d | %s: %d", len(ctxList), resType, resourceCount))
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
	currentNodes = nil
	currentNamespaces = nil
	currentPods = nil
	currentDeployments = nil
	currentStatefulSets = nil
	currentDaemonSets = nil
	currentReplicaSets = nil
	currentJobs = nil
	currentCronJobs = nil
	selectedResourceType = "Nodes"
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
	displayResourceList()
	logDebug("Виклик оновлення UI віджетів після завантаження")
	updateUIWidgets()
	logInfo("Завантаження та оновлення стану завершено.")
}

// --- Завантаження вибраних ресурсів ---
func loadSelectedResources() {
	stateMu.RLock()
	clientset := currentClientset
	resType := selectedResourceType
	contextName := connectedContextName
	stateMu.RUnlock()
	if clientset == nil {
		logWarning("Спроба завантажити ресурси без clientset.")
		if statusBar != nil {
			statusBar.SetText("Не підключено.")
		}
		stateMu.Lock()
		currentNodes = nil
		currentNamespaces = nil
		currentPods = nil
		currentDeployments = nil
		currentStatefulSets = nil
		currentDaemonSets = nil
		currentReplicaSets = nil
		currentJobs = nil
		currentCronJobs = nil
		stateMu.Unlock()
		displayResourceList()
		updateUIWidgets()
		return
	}
	logInfo("Завантаження ресурсів типу '%s' для '%s'", resType, contextName)
	if statusBar != nil {
		statusBar.SetText(fmt.Sprintf("Завантаження %s для '%s'...", resType, getDisplayName(contextName)))
	}
	var err error
	var statusMsg string = "OK"
	newNodes := []corev1.Node{}
	newNamespaces := []corev1.Namespace{}
	newPods := []corev1.Pod{}
	newDeployments := []appsv1.Deployment{}
	newStatefulSets := []appsv1.StatefulSet{}
	newDaemonSets := []appsv1.DaemonSet{}
	newReplicaSets := []appsv1.ReplicaSet{}
	newJobs := []batchv1.Job{}
	newCronJobs := []batchv1.CronJob{}
	listOptions := metav1.ListOptions{}
	ctxTimeout, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stateMu.Lock()
	currentNodes = nil
	currentNamespaces = nil
	currentPods = nil
	currentDeployments = nil
	currentStatefulSets = nil
	currentDaemonSets = nil
	currentReplicaSets = nil
	currentJobs = nil
	currentCronJobs = nil
	stateMu.Unlock()
	switch resType {
	case "Namespaces":
		list, listErr := clientset.CoreV1().Namespaces().List(ctxTimeout, listOptions)
		if listErr != nil {
			err = listErr
		} else {
			logDebug("OK: %d Ns", len(list.Items))
			newNamespaces = list.Items
			sort.Slice(newNamespaces, func(i, j int) bool { return newNamespaces[i].Name < newNamespaces[j].Name })
		}
	case "Nodes":
		list, listErr := clientset.CoreV1().Nodes().List(ctxTimeout, listOptions)
		if listErr != nil {
			err = listErr
		} else {
			logDebug("OK: %d Nodes", len(list.Items))
			newNodes = list.Items
			sort.Slice(newNodes, func(i, j int) bool { return newNodes[i].Name < newNodes[j].Name })
		}
	case "Pods":
		logWarning("ЗАВАНТАЖЕННЯ ВСІХ PODS!")
		list, listErr := clientset.CoreV1().Pods("").List(ctxTimeout, listOptions)
		if listErr != nil {
			err = listErr
		} else {
			logDebug("OK: %d Pods", len(list.Items))
			newPods = list.Items
			sort.Slice(newPods, func(i, j int) bool { return newPods[i].Name < newPods[j].Name })
		}
	case "Deployments":
		logWarning("ЗАВАНТАЖЕННЯ ВСІХ DEPLOYMENTS!")
		list, listErr := clientset.AppsV1().Deployments("").List(ctxTimeout, listOptions)
		if listErr != nil {
			err = listErr
		} else {
			logDebug("OK: %d Deploy", len(list.Items))
			newDeployments = list.Items
			sort.Slice(newDeployments, func(i, j int) bool { return newDeployments[i].Name < newDeployments[j].Name })
		}
	case "StatefulSets":
		logWarning("ЗАВАНТАЖЕННЯ ВСІХ STATEFULSETS!")
		list, listErr := clientset.AppsV1().StatefulSets("").List(ctxTimeout, listOptions)
		if listErr != nil {
			err = listErr
		} else {
			logDebug("OK: %d Sts", len(list.Items))
			newStatefulSets = list.Items
			sort.Slice(newStatefulSets, func(i, j int) bool { return newStatefulSets[i].Name < newStatefulSets[j].Name })
		}
	case "DaemonSets":
		logWarning("ЗАВАНТАЖЕННЯ ВСІХ DAEMONSETS!")
		list, listErr := clientset.AppsV1().DaemonSets("").List(ctxTimeout, listOptions)
		if listErr != nil {
			err = listErr
		} else {
			logDebug("OK: %d Ds", len(list.Items))
			newDaemonSets = list.Items
			sort.Slice(newDaemonSets, func(i, j int) bool { return newDaemonSets[i].Name < newDaemonSets[j].Name })
		}
	case "ReplicaSets":
		logWarning("ЗАВАНТАЖЕННЯ ВСІХ REPLICASETS!")
		list, listErr := clientset.AppsV1().ReplicaSets("").List(ctxTimeout, listOptions)
		if listErr != nil {
			err = listErr
		} else {
			logDebug("OK: %d Rs", len(list.Items))
			newReplicaSets = list.Items
			sort.Slice(newReplicaSets, func(i, j int) bool { return newReplicaSets[i].Name < newReplicaSets[j].Name })
		}
	case "Jobs":
		logWarning("ЗАВАНТАЖЕННЯ ВСІХ JOBS!")
		list, listErr := clientset.BatchV1().Jobs("").List(ctxTimeout, listOptions)
		if listErr != nil {
			err = listErr
		} else {
			logDebug("OK: %d Jobs", len(list.Items))
			newJobs = list.Items
			sort.Slice(newJobs, func(i, j int) bool { return newJobs[i].Name < newJobs[j].Name })
		}
	case "CronJobs":
		logWarning("ЗАВАНТАЖЕННЯ ВСІХ CRONJOBS!")
		list, listErr := clientset.BatchV1().CronJobs("").List(ctxTimeout, listOptions)
		if listErr != nil {
			err = listErr
		} else {
			logDebug("OK: %d Cj", len(list.Items))
			newCronJobs = list.Items
			sort.Slice(newCronJobs, func(i, j int) bool { return newCronJobs[i].Name < newCronJobs[j].Name })
		}
	default:
		logWarning("Невідомий тип ресурсу: %s", resType)
		err = fmt.Errorf("тип %s не підтримується", resType)
	}
	if err != nil {
		logError("Помилка завантаження %s: %v", resType, err)
		statusMsg = fmt.Sprintf("Помилка %s: %v", resType, err)
	}
	stateMu.Lock()
	switch resType {
	case "Namespaces":
		currentNamespaces = newNamespaces
	case "Nodes":
		currentNodes = newNodes
	case "Pods":
		currentPods = newPods
	case "Deployments":
		currentDeployments = newDeployments
	case "StatefulSets":
		currentStatefulSets = newStatefulSets
	case "DaemonSets":
		currentDaemonSets = newDaemonSets
	case "ReplicaSets":
		currentReplicaSets = newReplicaSets
	case "Jobs":
		currentJobs = newJobs
	case "CronJobs":
		currentCronJobs = newCronJobs
	}
	stateMu.Unlock()
	if statusBar != nil {
		if err == nil {
			count := 0
			switch resType {
			case "Namespaces":
				count = len(newNamespaces)
			case "Nodes":
				count = len(newNodes)
			case "Pods":
				count = len(newPods)
			case "Deployments":
				count = len(newDeployments)
			case "StatefulSets":
				count = len(newStatefulSets)
			case "DaemonSets":
				count = len(newDaemonSets)
			case "ReplicaSets":
				count = len(newReplicaSets)
			case "Jobs":
				count = len(newJobs)
			case "CronJobs":
				count = len(newCronJobs)
			}
			statusMsg = fmt.Sprintf("Підключено: %s | %s: %d", getDisplayName(contextName), resType, count)
		}
		statusBar.SetText(statusMsg)
	}
	displayResourceList()
	updateUIWidgets()
	logInfo("Завантаження '%s' завершено.", resType)
}

// Обгортка для підключення та початкового завантаження ресурсів
func connectLoadAndRefresh(ctxName string) {
	displayResourceList()
	if resourceListWidget != nil {
		resourceListWidget.Refresh()
	}
	clientset, _, err := connectToCluster(ctxName)
	stateMu.Lock()
	if err == nil {
		connectedContextName = ctxName
		currentClientset = clientset
		selectedResourceType = "Nodes"
		currentNodes = nil
		currentNamespaces = nil
		currentPods = nil
		currentDeployments = nil
		currentStatefulSets = nil
		currentDaemonSets = nil
		currentReplicaSets = nil
		currentJobs = nil
		currentCronJobs = nil
		logDebug("Збережено clientset: %s, вибрано тип: %s", ctxName, selectedResourceType)
		stateMu.Unlock()
		go loadSelectedResources()
	} else {
		connectedContextName = ""
		currentClientset = nil
		selectedResourceType = ""
		currentNodes = nil
		currentNamespaces = nil
		currentPods = nil
		currentDeployments = nil
		currentStatefulSets = nil
		currentDaemonSets = nil
		currentReplicaSets = nil
		currentJobs = nil
		currentCronJobs = nil
		logDebug("Помилка підключення.")
		stateMu.Unlock()
		displayResourceList()
		updateUIWidgets()
	}
}

// --- Функції для перемикання вмісту правої панелі ---
func displayResourceList() {
	logDebug("Показ списку ресурсів")
	if rightPanelContainer != nil && resourceListWidget != nil {
		resourceListWidget.Refresh()
		if len(rightPanelContainer.Objects) == 0 || rightPanelContainer.Objects[0] != resourceListWidget {
			rightPanelContainer.Objects = []fyne.CanvasObject{resourceListWidget}
			rightPanelContainer.Refresh()
		}
	} else {
		logError("rightPanelContainer або resourceListWidget є nil при показі списку")
	}
}

// --- Функції для показу деталей ресурсів ---
func createDetailRow(key string, value string) *fyne.Container {
	keyLabel := widget.NewLabelWithStyle(key+":", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	valueLabel := widget.NewLabel(value)
	valueLabel.Wrapping = fyne.TextWrapWord
	return container.NewBorder(nil, nil, keyLabel, nil, valueLabel)
}
func buildNodeDetailsView(node corev1.Node) fyne.CanvasObject {
	logDebug("Створення деталей для Node: %s", node.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", node.Name))
	detailsVBox.Add(createDetailRow("Created", node.CreationTimestamp.Format(time.RFC1123)))
	statusStr := ""
	for _, cond := range node.Status.Conditions {
		if cond.Status == corev1.ConditionTrue {
			statusStr += string(cond.Type) + " "
		}
	}
	if statusStr == "" {
		statusStr = "Unknown"
	}
	detailsVBox.Add(createDetailRow("Status", strings.TrimSpace(statusStr)))
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
	detailsVBox.Add(createDetailRow("Roles", strings.Join(roles, ", ")))
	detailsVBox.Add(widget.NewSeparator())
	detailsVBox.Add(createDetailRow("Kubelet Version", node.Status.NodeInfo.KubeletVersion))
	detailsVBox.Add(createDetailRow("OS Image", node.Status.NodeInfo.OSImage))
	detailsVBox.Add(createDetailRow("Kernel Version", node.Status.NodeInfo.KernelVersion))
	detailsVBox.Add(createDetailRow("Container Runtime", node.Status.NodeInfo.ContainerRuntimeVersion))
	detailsVBox.Add(widget.NewSeparator())
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
	detailsVBox.Add(createDetailRow("Internal IP", internalIP))
	detailsVBox.Add(createDetailRow("External IP", externalIP))
	detailsVBox.Add(widget.NewSeparator())
	cpu := node.Status.Capacity[corev1.ResourceCPU]
	mem := node.Status.Capacity[corev1.ResourceMemory]
	pods := node.Status.Capacity[corev1.ResourcePods]
	detailsVBox.Add(createDetailRow("CPU (Capacity)", cpu.String()))
	detailsVBox.Add(createDetailRow("Memory (Capacity)", mem.String()))
	detailsVBox.Add(createDetailRow("Pods (Capacity)", pods.String()))
	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildPodDetailsView(pod corev1.Pod) fyne.CanvasObject {
	logDebug("Створення деталей для Pod: %s/%s", pod.Namespace, pod.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", pod.Name))
	detailsVBox.Add(createDetailRow("Namespace", pod.Namespace))
	detailsVBox.Add(createDetailRow("Created", pod.CreationTimestamp.Format(time.RFC1123)))
	detailsVBox.Add(createDetailRow("Status", string(pod.Status.Phase)))
	if pod.Status.Reason != "" {
		detailsVBox.Add(createDetailRow("Reason", pod.Status.Reason))
	}
	detailsVBox.Add(createDetailRow("Pod IP", pod.Status.PodIP))
	detailsVBox.Add(createDetailRow("Node", pod.Spec.NodeName))
	owners := []string{}
	for _, owner := range pod.OwnerReferences {
		owners = append(owners, fmt.Sprintf("%s/%s", owner.Kind, owner.Name))
	}
	if len(owners) > 0 {
		detailsVBox.Add(createDetailRow("Controlled By", strings.Join(owners, ", ")))
	}
	detailsVBox.Add(widget.NewSeparator())
	detailsVBox.Add(widget.NewLabelWithStyle("Containers:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))
	containerBox := container.NewVBox()
	for _, cs := range pod.Status.ContainerStatuses {
		status := fmt.Sprintf(" - %s (Restarts: %d, Ready: %v)\n   Image: %s", cs.Name, cs.RestartCount, cs.Ready, cs.Image)
		contLabel := widget.NewLabel(status)
		contLabel.Wrapping = fyne.TextWrapWord
		containerBox.Add(contLabel)
		containerBox.Add(widget.NewSeparator())
	}
	if len(pod.Status.ContainerStatuses) == 0 {
		containerBox.Add(widget.NewLabel("  <none>"))
	}
	detailsVBox.Add(containerBox)
	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildNamespaceDetailsView(ns corev1.Namespace) fyne.CanvasObject {
	logDebug("Створення деталей для Namespace: %s", ns.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", ns.Name))
	detailsVBox.Add(createDetailRow("Created", ns.CreationTimestamp.Format(time.RFC1123)))
	detailsVBox.Add(createDetailRow("Status", string(ns.Status.Phase)))
	if len(ns.Labels) > 0 {
		detailsVBox.Add(widget.NewSeparator())
		detailsVBox.Add(widget.NewLabelWithStyle("Labels:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))
		keys := make([]string, 0, len(ns.Labels))
		for k := range ns.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			detailsVBox.Add(createDetailRow("  "+k, ns.Labels[k]))
		}
	}
	if len(ns.Annotations) > 0 {
		detailsVBox.Add(widget.NewSeparator())
		detailsVBox.Add(widget.NewLabelWithStyle("Annotations:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))
		keys := make([]string, 0, len(ns.Annotations))
		for k := range ns.Annotations {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			detailsVBox.Add(createDetailRow("  "+k, ns.Annotations[k]))
		}
	}
	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildDeploymentDetailsView(dep appsv1.Deployment) fyne.CanvasObject {
	logDebug("Створення деталей для Deployment: %s/%s", dep.Namespace, dep.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", dep.Name))
	detailsVBox.Add(createDetailRow("Namespace", dep.Namespace))
	detailsVBox.Add(createDetailRow("Created", dep.CreationTimestamp.Format(time.RFC1123)))
	detailsVBox.Add(createDetailRow("Replicas", fmt.Sprintf("%d desired, %d updated, %d total, %d available, %d unavailable", *dep.Spec.Replicas, dep.Status.UpdatedReplicas, dep.Status.Replicas, dep.Status.AvailableReplicas, dep.Status.UnavailableReplicas)))
	detailsVBox.Add(createDetailRow("Strategy", string(dep.Spec.Strategy.Type)))
	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildStatefulSetDetailsView(sts appsv1.StatefulSet) fyne.CanvasObject {
	logDebug("Створення деталей для StatefulSet: %s/%s", sts.Namespace, sts.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", sts.Name))
	detailsVBox.Add(createDetailRow("Namespace", sts.Namespace))
	detailsVBox.Add(createDetailRow("Created", sts.CreationTimestamp.Format(time.RFC1123)))
	detailsVBox.Add(createDetailRow("Replicas", fmt.Sprintf("%d desired, %d current, %d ready", *sts.Spec.Replicas, sts.Status.CurrentReplicas, sts.Status.ReadyReplicas)))
	detailsVBox.Add(createDetailRow("Service Name", sts.Spec.ServiceName))
	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildDaemonSetDetailsView(ds appsv1.DaemonSet) fyne.CanvasObject {
	logDebug("Створення деталей для DaemonSet: %s/%s", ds.Namespace, ds.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", ds.Name))
	detailsVBox.Add(createDetailRow("Namespace", ds.Namespace))
	detailsVBox.Add(createDetailRow("Created", ds.CreationTimestamp.Format(time.RFC1123)))
	detailsVBox.Add(createDetailRow("Pods", fmt.Sprintf("%d desired, %d current, %d ready, %d available, %d unavailable", ds.Status.DesiredNumberScheduled, ds.Status.CurrentNumberScheduled, ds.Status.NumberReady, ds.Status.NumberAvailable, ds.Status.NumberUnavailable)))
	detailsVBox.Add(createDetailRow("Update Strategy", string(ds.Spec.UpdateStrategy.Type)))
	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildReplicaSetDetailsView(rs appsv1.ReplicaSet) fyne.CanvasObject {
	logDebug("Створення деталей для ReplicaSet: %s/%s", rs.Namespace, rs.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", rs.Name))
	detailsVBox.Add(createDetailRow("Namespace", rs.Namespace))
	detailsVBox.Add(createDetailRow("Created", rs.CreationTimestamp.Format(time.RFC1123)))
	detailsVBox.Add(createDetailRow("Replicas", fmt.Sprintf("%d desired, %d current, %d ready", *rs.Spec.Replicas, rs.Status.Replicas, rs.Status.ReadyReplicas)))
	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildJobDetailsView(job batchv1.Job) fyne.CanvasObject {
	logDebug("Створення деталей для Job: %s/%s", job.Namespace, job.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", job.Name))
	detailsVBox.Add(createDetailRow("Namespace", job.Namespace))
	detailsVBox.Add(createDetailRow("Created", job.CreationTimestamp.Format(time.RFC1123)))
	completions := "N/A"
	if job.Spec.Completions != nil {
		completions = fmt.Sprintf("%d", *job.Spec.Completions)
	}
	detailsVBox.Add(createDetailRow("Completions", completions))
	parallelism := "N/A"
	if job.Spec.Parallelism != nil {
		parallelism = fmt.Sprintf("%d", *job.Spec.Parallelism)
	}
	detailsVBox.Add(createDetailRow("Parallelism", parallelism))
	detailsVBox.Add(createDetailRow("Status", fmt.Sprintf("%d active, %d succeeded, %d failed", job.Status.Active, job.Status.Succeeded, job.Status.Failed)))
	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildCronJobDetailsView(cj batchv1.CronJob) fyne.CanvasObject {
	logDebug("Створення деталей для CronJob: %s/%s", cj.Namespace, cj.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", cj.Name))
	detailsVBox.Add(createDetailRow("Namespace", cj.Namespace))
	detailsVBox.Add(createDetailRow("Created", cj.CreationTimestamp.Format(time.RFC1123)))
	detailsVBox.Add(createDetailRow("Schedule", cj.Spec.Schedule))
	suspend := "False"
	if cj.Spec.Suspend != nil && *cj.Spec.Suspend {
		suspend = "True"
	}
	detailsVBox.Add(createDetailRow("Suspend", suspend))
	detailsVBox.Add(createDetailRow("Active Jobs", fmt.Sprintf("%d", len(cj.Status.Active))))
	lastSchedule := "Never"
	if cj.Status.LastScheduleTime != nil {
		lastSchedule = cj.Status.LastScheduleTime.Format(time.RFC1123)
	}
	detailsVBox.Add(createDetailRow("Last Schedule", lastSchedule))
	detailsVBox.Add(createDetailRow("Concurrency Policy", string(cj.Spec.ConcurrencyPolicy)))
	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}

// Виправлено: Перейменовано функцію та змінено тип повернення
func buildNotImplementedDetailsView(resourceType, resourceName string) fyne.CanvasObject {
	logDebug("Створення заглушки для деталей: %s %s", resourceType, resourceName)
	detailsVBox := container.NewVBox()
	label := widget.NewLabel(fmt.Sprintf("Детальний вигляд для типу '%s' ('%s') ще не реалізовано.", resourceType, resourceName))
	label.Wrapping = fyne.TextWrapWord
	label.Alignment = fyne.TextAlignCenter
	detailsVBox.Add(label)
	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	// Використовуємо Border для консистентності
	return container.NewBorder(backButton, nil, nil, nil, container.NewPadded(detailsVBox))
}

// Виправлено: Перейменовано функцію та змінено тип повернення
func buildErrorDetailsView(resourceType, resourceName string, err error) fyne.CanvasObject {
	detailsVBox := container.NewVBox()
	label := widget.NewLabel(fmt.Sprintf("Помилка завантаження деталей для %s '%s':\n%v", resourceType, resourceName, err))
	label.Wrapping = fyne.TextWrapWord
	label.Alignment = fyne.TextAlignCenter
	detailsVBox.Add(label)
	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewPadded(detailsVBox))
}

// --- Допоміжні функції ---
// (initializeLoadingRules, openKubeFolder без змін)
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
// (Без змін)
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
// (Без змін)
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
	selectedResourceType = "Nodes"

	// --- Налаштування трея (закоментовано) ---
	// ...

	// --- Створюємо UI елементи ---
	currentContextLabel = widget.NewLabel(labelLoading)
	statusBar = widget.NewLabel("Ініціалізація...")

	contextListWidget = widget.NewList( /* ... */
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
	contextListWidget.OnSelected = func(id widget.ListItemID) { /* ... */
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

	resourceTypeTree = widget.NewTree( /* ... */
		func(id widget.TreeNodeID) []widget.TreeNodeID {
			children, _ := resourceTreeData[id]
			sort.Strings(children)
			return children
		},
		func(id widget.TreeNodeID) bool {
			_, ok := resourceTreeData[id]
			return (id == "" || ok && len(resourceTreeData[id]) > 0) && !resourceLeafNodes[id]
		},
		func(branch bool) fyne.CanvasObject {
			if branch {
				return container.NewHBox(widget.NewIcon(theme.FolderIcon()), widget.NewLabel("Template Group"))
			}
			return widget.NewLabel("Template Resource")
		},
		func(id widget.TreeNodeID, branch bool, node fyne.CanvasObject) {
			if branch {
				if cont, ok := node.(*fyne.Container); ok && len(cont.Objects) > 1 {
					if lbl, ok2 := cont.Objects[1].(*widget.Label); ok2 {
						lbl.SetText(id)
					}
				}
			} else {
				if lbl, ok := node.(*widget.Label); ok {
					lbl.SetText(id)
				}
			}
		},
	)
	resourceTypeTree.OnSelected = func(id widget.TreeNodeID) { /* ... */
		logInfo("Вибрано вузол дерева ресурсів: %s", id)
		if resourceLeafNodes[id] {
			logInfo("Це листовий вузол - тип ресурсу: %s", id)
			stateMu.Lock()
			currentResType := selectedResourceType
			stateMu.Unlock()
			if id != currentResType {
				stateMu.Lock()
				selectedResourceType = id
				stateMu.Unlock()
				go loadSelectedResources()
			}
		} else {
			logDebug("Вибрано групу '%s', розгортаємо/згортаємо.", id)
			if resourceTypeTree.IsBranchOpen(id) {
				resourceTypeTree.CloseBranch(id)
			} else {
				resourceTypeTree.OpenBranch(id)
			}
		}
	}
	resourceTypeTree.OpenBranch("Cluster")
	resourceTypeTree.OpenBranch("Workloads")

	resourceListWidget = widget.NewList( /* ... */
		func() int {
			stateMu.RLock()
			defer stateMu.RUnlock()
			switch selectedResourceType {
			case "Namespaces":
				return len(currentNamespaces)
			case "Nodes":
				return len(currentNodes)
			case "Pods":
				return len(currentPods)
			case "Deployments":
				return len(currentDeployments)
			case "StatefulSets":
				return len(currentStatefulSets)
			case "DaemonSets":
				return len(currentDaemonSets)
			case "ReplicaSets":
				return len(currentReplicaSets)
			case "Jobs":
				return len(currentJobs)
			case "CronJobs":
				return len(currentCronJobs)
			default:
				return 0
			}
		},
		func() fyne.CanvasObject { return widget.NewLabel("template resource") },
		func(id widget.ListItemID, item fyne.CanvasObject) {
			stateMu.RLock()
			name := ""
			resType := selectedResourceType
			switch resType {
			case "Namespaces":
				if id >= 0 && id < len(currentNamespaces) {
					name = currentNamespaces[id].Name
				}
			case "Nodes":
				if id >= 0 && id < len(currentNodes) {
					name = currentNodes[id].Name
				}
			case "Pods":
				if id >= 0 && id < len(currentPods) {
					name = fmt.Sprintf("%s/%s", currentPods[id].Namespace, currentPods[id].Name)
				}
			case "Deployments":
				if id >= 0 && id < len(currentDeployments) {
					name = fmt.Sprintf("%s/%s", currentDeployments[id].Namespace, currentDeployments[id].Name)
				}
			case "StatefulSets":
				if id >= 0 && id < len(currentStatefulSets) {
					name = fmt.Sprintf("%s/%s", currentStatefulSets[id].Namespace, currentStatefulSets[id].Name)
				}
			case "DaemonSets":
				if id >= 0 && id < len(currentDaemonSets) {
					name = fmt.Sprintf("%s/%s", currentDaemonSets[id].Namespace, currentDaemonSets[id].Name)
				}
			case "ReplicaSets":
				if id >= 0 && id < len(currentReplicaSets) {
					name = fmt.Sprintf("%s/%s", currentReplicaSets[id].Namespace, currentReplicaSets[id].Name)
				}
			case "Jobs":
				if id >= 0 && id < len(currentJobs) {
					name = fmt.Sprintf("%s/%s", currentJobs[id].Namespace, currentJobs[id].Name)
				}
			case "CronJobs":
				if id >= 0 && id < len(currentCronJobs) {
					name = fmt.Sprintf("%s/%s", currentCronJobs[id].Namespace, currentCronJobs[id].Name)
				}
			}
			stateMu.RUnlock()
			item.(*widget.Label).SetText(name)
		},
	)
	// Оновлено OnSelected для виклику ПРАВИЛЬНИХ функцій деталей
	resourceListWidget.OnSelected = func(id widget.ListItemID) {
		stateMu.RLock()
		resType := selectedResourceType
		var detailWidget fyne.CanvasObject // Віджет, який буде показано
		var resourceName string            // Для логування та заглушки

		// Отримуємо об'єкт та викликаємо відповідну функцію побудови
		switch resType {
		case "Namespaces":
			if id >= 0 && id < len(currentNamespaces) {
				obj := currentNamespaces[id]
				resourceName = obj.Name
				detailWidget = buildNamespaceDetailsView(obj)
			}
		case "Nodes":
			if id >= 0 && id < len(currentNodes) {
				obj := currentNodes[id]
				resourceName = obj.Name
				detailWidget = buildNodeDetailsView(obj)
			}
		case "Pods":
			if id >= 0 && id < len(currentPods) {
				obj := currentPods[id]
				resourceName = obj.Name
				detailWidget = buildPodDetailsView(obj)
			}
		case "Deployments":
			if id >= 0 && id < len(currentDeployments) {
				obj := currentDeployments[id]
				resourceName = obj.Name
				detailWidget = buildDeploymentDetailsView(obj)
			}
		case "StatefulSets":
			if id >= 0 && id < len(currentStatefulSets) {
				obj := currentStatefulSets[id]
				resourceName = obj.Name
				detailWidget = buildStatefulSetDetailsView(obj)
			}
		case "DaemonSets":
			if id >= 0 && id < len(currentDaemonSets) {
				obj := currentDaemonSets[id]
				resourceName = obj.Name
				detailWidget = buildDaemonSetDetailsView(obj)
			}
		case "ReplicaSets":
			if id >= 0 && id < len(currentReplicaSets) {
				obj := currentReplicaSets[id]
				resourceName = obj.Name
				detailWidget = buildReplicaSetDetailsView(obj)
			}
		case "Jobs":
			if id >= 0 && id < len(currentJobs) {
				obj := currentJobs[id]
				resourceName = obj.Name
				detailWidget = buildJobDetailsView(obj)
			}
		case "CronJobs":
			if id >= 0 && id < len(currentCronJobs) {
				obj := currentCronJobs[id]
				resourceName = obj.Name
				detailWidget = buildCronJobDetailsView(obj)
			}
		default:
			logWarning("Вибрано ресурс невідомого типу '%s' для деталей", resType)
		}
		stateMu.RUnlock()

		if detailWidget != nil { // Якщо віджет деталей створено
			fullName := resourceName
			// Якщо потрібен неймспейс для логу/статусу, його треба отримати разом з об'єктом вище
			// stateMu.RLock(); if ns != "" { fullName = ns + "/" + resourceName }; stateMu.RUnlock() // Приклад
			logInfo("Вибрано ресурс '%s': %s", resType, fullName)
			if statusBar != nil {
				statusBar.SetText(fmt.Sprintf("Вибрано %s: %s", resType, fullName))
			}
			// Оновлюємо праву панель
			if rightPanelContainer != nil {
				rightPanelContainer.Objects = []fyne.CanvasObject{detailWidget}
				rightPanelContainer.Refresh()
			}
		} else if resourceName != "" { // Якщо об'єкт не вдалося отримати, але ім'я є
			logWarning("Не вдалося отримати об'єкт для '%s': %s", resType, resourceName)
			detailWidget = buildNotImplementedDetailsView(resType, resourceName) // Показуємо заглушку
			if rightPanelContainer != nil {
				rightPanelContainer.Objects = []fyne.CanvasObject{detailWidget}
				rightPanelContainer.Refresh()
			}
		} else {
			logWarning("Не вдалося отримати ідентифікатор/об'єкт для вибраного ресурсу типу '%s', ID: %d", resType, id)
		}
	}

	// --- Збираємо макет вікна ---
	leftPanelContent := container.NewVSplit(container.NewBorder(container.NewPadded(widget.NewLabel("Контексти:")), nil, nil, nil, contextListWidget), container.NewBorder(container.NewPadded(widget.NewLabel("Ресурси:")), nil, nil, nil, resourceTypeTree))
	leftPanelContent.Offset = 0.5
	rightPanelContainer = container.NewMax(resourceListWidget)
	tappableRightPanel := &tappableContainer{content: rightPanelContainer}
	tappableRightPanel.ExtendBaseWidget(tappableRightPanel)
	split := container.NewHSplit(leftPanelContent, tappableRightPanel)
	split.Offset = 0.3
	mainLayout := container.NewBorder(container.NewVBox(currentContextLabel, widget.NewSeparator()), statusBar, nil, nil, split)
	mainWindow.SetContent(mainLayout)

	mainWindow.Resize(fyne.NewSize(900, 700))
	mainWindow.CenterOnScreen()
	mainWindow.SetCloseIntercept(func() { logInfo("Закриття вікна..."); fyneApp.Quit() })

	go loadAndUpdateState()
	mainWindow.ShowAndRun()
	logInfo(logPrefix + " завершено.")
}
