package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"github.com/fsnotify/fsnotify"
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
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
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
const logPrefix = "GoKubeLens(Step8-Network)" // Оновлено префікс
const maxContextItems = 50
const labelLoading = "Завантаження..."
const labelError = "Помилка"
const labelNoContext = "Немає контексту"
const labelBackToList = "<- Назад до списку"
const appTitle = "Go Kube Manager (Lens Clone) - Step 8"

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

	resourceTreeData  = map[resourceTreeNodeID][]resourceTreeNodeID{"": {"Cluster", "Workloads", "Network", "Storage", "Configuration", "Access Control"}, "Cluster": {"Namespaces", "Nodes"}, "Workloads": {"Pods", "Deployments", "StatefulSets", "DaemonSets", "ReplicaSets", "Jobs", "CronJobs"}, "Network": {"Services", "Ingresses"}, "Storage": {"PersistentVolumes", "PersistentVolumeClaims", "StorageClasses"}, "Configuration": {"ConfigMaps", "Secrets"}, "Access Control": {"ServiceAccounts", "Roles", "RoleBindings", "ClusterRoles", "ClusterRoleBindings"}}
	resourceLeafNodes = map[resourceTreeNodeID]bool{"Namespaces": true, "Nodes": true, "Pods": true, "Deployments": true, "StatefulSets": true, "DaemonSets": true, "ReplicaSets": true, "Jobs": true, "CronJobs": true, "Services": true, "Ingresses": true, "PersistentVolumes": true, "PersistentVolumeClaims": true, "StorageClasses": true, "ConfigMaps": true, "Secrets": true, "ServiceAccounts": true, "Roles": true, "RoleBindings": true, "ClusterRoles": true, "ClusterRoleBindings": true}
	arnRegex          = regexp.MustCompile(`^arn:aws:eks:[^:]+:(\d+):cluster/(.+)$`)

	currentContextName   string
	connectedContextName string
	allContextNames      []string
	selectedResourceType string
	// Списки ресурсів
	currentNodes        []corev1.Node
	currentNamespaces   []corev1.Namespace
	currentPods         []corev1.Pod
	currentDeployments  []appsv1.Deployment
	currentStatefulSets []appsv1.StatefulSet
	currentDaemonSets   []appsv1.DaemonSet
	currentReplicaSets  []appsv1.ReplicaSet
	currentJobs         []batchv1.Job
	currentCronJobs     []batchv1.CronJob
	currentConfigMaps   []corev1.ConfigMap
	currentSecrets      []corev1.Secret
	currentServices     []corev1.Service
	currentIngresses    []networkingv1.Ingress
	// Access Control
	currentServiceAccounts     []corev1.ServiceAccount
	currentRoles               []rbacv1.Role
	currentRoleBindings        []rbacv1.RoleBinding
	currentClusterRoles        []rbacv1.ClusterRole
	currentClusterRoleBindings []rbacv1.ClusterRoleBinding
	// Storage
	currentPersistentVolumes      []corev1.PersistentVolume
	currentPersistentVolumeClaims []corev1.PersistentVolumeClaim
	currentStorageClasses         []storagev1.StorageClass

	kubeconfigFile     string
	isKubeconfigEnvSet bool
	loadingRules       clientcmd.ClientConfigLoadingRules
	currentClientset   *kubernetes.Clientset
	stateMu            sync.RWMutex

	// Моніторинг файлу
	watcher     *fsnotify.Watcher
	watcherDone chan bool
)

// Мапи для дерева ресурсів
type resourceTreeNodeID = string
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

// Змінює current-context у файлі kubeconfig
func switchContext(contextName string) error {
	logDebug("Перемикання default context на '%s' у файлі...", contextName)

	stateMu.RLock()
	configPath := kubeconfigFile // Використовуємо визначений шлях
	stateMu.RUnlock()

	if configPath == "" {
		return errors.New("шлях до kubeconfig не визначено")
	}

	// Завантажуємо поточну конфігурацію безпосередньо з файлу
	config, err := clientcmd.LoadFromFile(configPath)
	if err != nil {
		logError("Не вдалося завантажити '%s' для зміни: %v", configPath, err)
		// Якщо файл не знайдено, помилка все одно виникне при записі,
		// але можна додати явну перевірку os.IsNotExist(err) тут, якщо потрібно
		return fmt.Errorf("неможливо завантажити %s: %w", configPath, err)
	}

	// Перевіряємо, чи існує контекст, на який перемикаємось
	if _, exists := config.Contexts[contextName]; !exists {
		logError("Спроба перемкнутись на неіснуючий контекст: %s", contextName)
		return fmt.Errorf("контекст '%s' не знайдено у %s", contextName, configPath)
	}

	if config.CurrentContext == contextName {
		logInfo("Контекст '%s' вже є поточним у файлі.", contextName)
		return nil // Нічого не робимо
	}

	// Змінюємо поточний контекст у структурі
	config.CurrentContext = contextName
	logDebug("Встановлено CurrentContext = '%s' у структурі для запису.", contextName)

	// Записуємо змінену конфігурацію назад у файл
	logDebug("Спроба зберегти змінену конфігурацію у файл: %s", configPath)
	err = clientcmd.WriteToFile(*config, configPath)
	if err != nil {
		logError("Не вдалося записати змінену конфігурацію у файл '%s': %v", configPath, err)
		return fmt.Errorf("помилка збереження конфігурації: %w", err)
	}

	logInfo("Успішно змінено default context на '%s' у файлі %s", contextName, configPath)
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
	//connCtx := connectedContextName
	ctxList := allContextNames
	resType := selectedResourceType
	statusMsg := ""
	if statusBar != nil {
		statusMsg = statusBar.Text
	}
	// Отримуємо кількість для всіх типів
	nodesCount := len(currentNodes)
	nsCount := len(currentNamespaces)
	podsCount := len(currentPods)
	deployCount := len(currentDeployments)
	stsCount := len(currentStatefulSets)
	dsCount := len(currentDaemonSets)
	rsCount := len(currentReplicaSets)
	jobCount := len(currentJobs)
	cronJobCount := len(currentCronJobs)
	cmCount := len(currentConfigMaps)
	secretCount := len(currentSecrets)
	svcCount := len(currentServices)
	ingCount := len(currentIngresses)
	// Access Control Counts
	saCount := len(currentServiceAccounts)
	roleCount := len(currentRoles)
	roleBindingCount := len(currentRoleBindings)
	clusterRoleCount := len(currentClusterRoles)
	clusterRoleBindingCount := len(currentClusterRoleBindings)
	// Storage Counts
	pvCount := len(currentPersistentVolumes)
	pvcCount := len(currentPersistentVolumeClaims)
	scCount := len(currentStorageClasses)

	stateMu.RUnlock()

	displayCtxFromFile := labelNoContext
	if ctxFromFile != "" {
		displayCtxFromFile = getDisplayName(ctxFromFile)
	}
	if currentContextLabel != nil {
		currentContextLabel.SetText("Поточний у файлі: " + displayCtxFromFile)
	}

	if contextListWidget != nil { /* ... оновлення списку контекстів ... */
		contextListWidget.Refresh()
		//targetSelection := connCtx
		//if targetSelection == "" {
		//	targetSelection = ctxFromFile
		//}
		//selectedIndex := -1
		//for i, name := range ctxList {
		//	if name == targetSelection {
		//		selectedIndex = i
		//		break
		//	}
		//}
		//if selectedIndex != -1 {
		//	contextListWidget.Select(selectedIndex)
		//} else {
		//	contextListWidget.UnselectAll()
		//}
	}
	if resourceTypeTree != nil { /* ... оновлення дерева ... */
		resourceTypeTree.Refresh()
		if resType != "" {
			resourceTypeTree.Select(resType)
		} else {
			resourceTypeTree.UnselectAll()
		}
	}

	resourceCount := 0
	if resourceListWidget != nil {
		stateMu.RLock() // Потрібне блокування для читання кількості
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
		case "ConfigMaps":
			resourceCount = cmCount
		case "Secrets":
			resourceCount = secretCount
		case "Services":
			resourceCount = svcCount
		case "Ingresses":
			resourceCount = ingCount
		case "ServiceAccounts":
			resourceCount = saCount
		case "Roles":
			resourceCount = roleCount
		case "RoleBindings":
			resourceCount = roleBindingCount
		case "ClusterRoles":
			resourceCount = clusterRoleCount
		case "ClusterRoleBindings":
			resourceCount = clusterRoleBindingCount
		case "PersistentVolumes":
			resourceCount = pvCount
		case "PersistentVolumeClaims":
			resourceCount = pvcCount
		case "StorageClasses":
			resourceCount = scCount
		// Додайте інші типи тут...
		default:
			resourceCount = 0
		}
		stateMu.RUnlock()
		logDebug("Оновлення списку ресурсів '%s' у UI (%d елементів)", resType, resourceCount)
		resourceListWidget.Refresh()
	}

	updateSystemTrayMenu()

	if statusBar != nil && !strings.HasPrefix(statusMsg, "Помилка") && !strings.HasPrefix(statusMsg, "Підключення") && !strings.HasPrefix(statusMsg, "Завантаження") {
		// Оновлено рядок стану
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
	// Скидаємо ВСІ списки ресурсів
	currentNodes = nil
	currentNamespaces = nil
	currentPods = nil
	currentDeployments = nil
	currentStatefulSets = nil
	currentDaemonSets = nil
	currentReplicaSets = nil
	currentJobs = nil
	currentCronJobs = nil
	currentConfigMaps = nil
	currentSecrets = nil
	currentServices = nil
	currentIngresses = nil
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
		currentConfigMaps = nil
		currentSecrets = nil
		currentServices = nil
		currentIngresses = nil
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
	// Створюємо локальні змінні для результатів
	newNodes := []corev1.Node{}
	newNamespaces := []corev1.Namespace{}
	newPods := []corev1.Pod{}
	newDeployments := []appsv1.Deployment{}
	newStatefulSets := []appsv1.StatefulSet{}
	newDaemonSets := []appsv1.DaemonSet{}
	newReplicaSets := []appsv1.ReplicaSet{}
	newJobs := []batchv1.Job{}
	newCronJobs := []batchv1.CronJob{}
	newConfigMaps := []corev1.ConfigMap{}
	newSecrets := []corev1.Secret{}
	newServices := []corev1.Service{}
	newIngresses := []networkingv1.Ingress{}
	newServiceAccounts := []corev1.ServiceAccount{}
	newRoles := []rbacv1.Role{}
	newRoleBindings := []rbacv1.RoleBinding{}
	newClusterRoles := []rbacv1.ClusterRole{}
	newClusterRoleBindings := []rbacv1.ClusterRoleBinding{}
	// Storage
	newPersistentVolumes := []corev1.PersistentVolume{}
	newPersistentVolumeClaims := []corev1.PersistentVolumeClaim{}
	newStorageClasses := []storagev1.StorageClass{}
	listOptions := metav1.ListOptions{}
	ctxTimeout, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Очищуємо ВСІ глобальні списки перед заповненням одного
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
	currentConfigMaps = nil
	currentSecrets = nil
	currentServices = nil
	currentIngresses = nil
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
	case "ConfigMaps":
		logWarning("ЗАВАНТАЖЕННЯ ВСІХ CONFIGMAPS!")
		list, listErr := clientset.CoreV1().ConfigMaps("").List(ctxTimeout, listOptions)
		if listErr != nil {
			err = listErr
		} else {
			logDebug("OK: %d CM", len(list.Items))
			newConfigMaps = list.Items
			sort.Slice(newConfigMaps, func(i, j int) bool { return newConfigMaps[i].Name < newConfigMaps[j].Name })
		}
	case "Secrets":
		logWarning("ЗАВАНТАЖЕННЯ ВСІХ SECRETS!")
		list, listErr := clientset.CoreV1().Secrets("").List(ctxTimeout, listOptions)
		if listErr != nil {
			err = listErr
		} else {
			logDebug("OK: %d Secrets", len(list.Items))
			newSecrets = list.Items
			sort.Slice(newSecrets, func(i, j int) bool { return newSecrets[i].Name < newSecrets[j].Name })
		}
	// Services та Ingresses
	case "Services":
		logWarning("ЗАВАНТАЖЕННЯ ВСІХ SERVICES!")
		list, listErr := clientset.CoreV1().Services("").List(ctxTimeout, listOptions)
		if listErr != nil {
			err = listErr
		} else {
			logDebug("OK: %d Svc", len(list.Items))
			newServices = list.Items
			sort.Slice(newServices, func(i, j int) bool { return newServices[i].Name < newServices[j].Name })
		}
	case "Ingresses":
		logWarning("ЗАВАНТАЖЕННЯ ВСІХ INGRESSES!")
		list, listErr := clientset.NetworkingV1().Ingresses("").List(ctxTimeout, listOptions)
		if listErr != nil {
			err = listErr
		} else {
			logDebug("OK: %d Ing", len(list.Items))
			newIngresses = list.Items
			sort.Slice(newIngresses, func(i, j int) bool { return newIngresses[i].Name < newIngresses[j].Name })
		}
	case "ServiceAccounts":
		logWarning("ЗАВАНТАЖЕННЯ ВСІХ SERVICEACCOUNTS!")
		list, listErr := clientset.CoreV1().ServiceAccounts("").List(ctxTimeout, listOptions)
		if listErr != nil {
			err = listErr
		} else {
			logDebug("OK: %d ServiceAccounts", len(list.Items))
			newServiceAccounts = list.Items
			sort.Slice(newServiceAccounts, func(i, j int) bool { return newServiceAccounts[i].Name < newServiceAccounts[j].Name })
		}
	case "Roles":
		logWarning("ЗАВАНТАЖЕННЯ ВСІХ ROLES!")
		list, listErr := clientset.RbacV1().Roles("").List(ctxTimeout, listOptions)
		if listErr != nil {
			err = listErr
		} else {
			logDebug("OK: %d Roles", len(list.Items))
			newRoles = list.Items
			sort.Slice(newRoles, func(i, j int) bool { return newRoles[i].Name < newRoles[j].Name })
		}
	case "RoleBindings":
		logWarning("ЗАВАНТАЖЕННЯ ВСІХ ROLEBINDINGS!")
		list, listErr := clientset.RbacV1().RoleBindings("").List(ctxTimeout, listOptions)
		if listErr != nil {
			err = listErr
		} else {
			logDebug("OK: %d RoleBindings", len(list.Items))
			newRoleBindings = list.Items
			sort.Slice(newRoleBindings, func(i, j int) bool { return newRoleBindings[i].Name < newRoleBindings[j].Name })
		}
	case "ClusterRoles":
		logWarning("ЗАВАНТАЖЕННЯ ВСІХ CLUSTERROLES!")
		list, listErr := clientset.RbacV1().ClusterRoles().List(ctxTimeout, listOptions)
		if listErr != nil {
			err = listErr
		} else {
			logDebug("OK: %d ClusterRoles", len(list.Items))
			newClusterRoles = list.Items
			sort.Slice(newClusterRoles, func(i, j int) bool { return newClusterRoles[i].Name < newClusterRoles[j].Name })
		}
	case "ClusterRoleBindings":
		logWarning("ЗАВАНТАЖЕННЯ ВСІХ CLUSTERROLEBINDINGS!")
		list, listErr := clientset.RbacV1().ClusterRoleBindings().List(ctxTimeout, listOptions)
		if listErr != nil {
			err = listErr
		} else {
			logDebug("OK: %d ClusterRoleBindings", len(list.Items))
			newClusterRoleBindings = list.Items
			sort.Slice(newClusterRoleBindings, func(i, j int) bool { return newClusterRoleBindings[i].Name < newClusterRoleBindings[j].Name })
		}
	case "PersistentVolumes":
		logWarning("ЗАВАНТАЖЕННЯ ВСІХ PERSISTENTVOLUMES!")
		list, listErr := clientset.CoreV1().PersistentVolumes().List(ctxTimeout, listOptions)
		if listErr != nil {
			err = listErr
		} else {
			logDebug("OK: %d PersistentVolumes", len(list.Items))
			newPersistentVolumes = list.Items
			sort.Slice(newPersistentVolumes, func(i, j int) bool { return newPersistentVolumes[i].Name < newPersistentVolumes[j].Name })
		}
	case "PersistentVolumeClaims":
		logWarning("ЗАВАНТАЖЕННЯ ВСІХ PERSISTENTVOLUMECLAIMS!")
		list, listErr := clientset.CoreV1().PersistentVolumeClaims("").List(ctxTimeout, listOptions)
		if listErr != nil {
			err = listErr
		} else {
			logDebug("OK: %d PersistentVolumeClaims", len(list.Items))
			newPersistentVolumeClaims = list.Items
			sort.Slice(newPersistentVolumeClaims, func(i, j int) bool { return newPersistentVolumeClaims[i].Name < newPersistentVolumeClaims[j].Name })
		}
	case "StorageClasses":
		logWarning("ЗАВАНТАЖЕННЯ ВСІХ STORAGECLASSES!")
		list, listErr := clientset.StorageV1().StorageClasses().List(ctxTimeout, listOptions)
		if listErr != nil {
			err = listErr
		} else {
			logDebug("OK: %d StorageClasses", len(list.Items))
			newStorageClasses = list.Items
			sort.Slice(newStorageClasses, func(i, j int) bool { return newStorageClasses[i].Name < newStorageClasses[j].Name })
		}
	default:
		logWarning("Невідомий тип ресурсу: %s", resType)
		err = fmt.Errorf("тип %s не підтримується", resType)
	}

	if err != nil {
		logError("Помилка завантаження %s: %v", resType, err)
		statusMsg = fmt.Sprintf("Помилка %s: %v", resType, err)
	}

	// Оновлюємо глобальний стан
	stateMu.Lock()
	switch resType { // Зберігаємо ТІЛЬКИ завантажений тип
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
	case "ConfigMaps":
		currentConfigMaps = newConfigMaps
	case "Secrets":
		currentSecrets = newSecrets
	case "Services":
		currentServices = newServices
	case "Ingresses":
		currentIngresses = newIngresses
	case "ServiceAccounts":
		currentServiceAccounts = newServiceAccounts
	case "Roles":
		currentRoles = newRoles
	case "RoleBindings":
		currentRoleBindings = newRoleBindings
	case "ClusterRoles":
		currentClusterRoles = newClusterRoles
	case "ClusterRoleBindings":
		currentClusterRoleBindings = newClusterRoleBindings
	case "PersistentVolumes":
		currentPersistentVolumes = newPersistentVolumes
	case "PersistentVolumeClaims":
		currentPersistentVolumeClaims = newPersistentVolumeClaims
	case "StorageClasses":
		currentStorageClasses = newStorageClasses
	}
	stateMu.Unlock()

	// Оновлюємо статус бар
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
			case "ConfigMaps":
				count = len(newConfigMaps)
			case "Secrets":
				count = len(newSecrets)
			case "Services":
				count = len(newServices)
			case "Ingresses":
				count = len(newIngresses)
			}
			statusMsg = fmt.Sprintf("Підключено: %s | %s: %d", getDisplayName(contextName), resType, count)
		}
		statusBar.SetText(statusMsg)
	}

	displayResourceList()
	updateUIWidgets()
	logInfo("Завантаження '%s' завершено.", resType)
}

// Показує порожню заглушку або інструкцію
func displayEmptyOverview() {
	logDebug("Показ порожньої правої панелі")
	if rightPanelContainer != nil {
		// Створюємо новий центральний контейнер, щоб він не був nil
		// якщо раніше там був список
		placeholder := container.NewCenter(widget.NewLabel("Виберіть контекст та тип ресурсу"))
		// Переконуємося, що об'єкти оновлюються
		rightPanelContainer.Objects = []fyne.CanvasObject{placeholder}
		rightPanelContainer.Refresh()
	} else {
		logError("rightPanelContainer є nil при показі порожньої панелі")
	}
	// Також оновлюємо resourceListWidget, щоб він був порожнім
	if resourceListWidget != nil {
		resourceListWidget.Refresh()
	}
}

// Обгортка для підключення та початкового відображення огляду кластера
func connectLoadAndRefresh(ctxName string) {
	// Одразу показуємо порожню праву панель або заглушку
	displayEmptyOverview() // Нова функція, щоб очистити праву панель

	clientset, serverVersion, err := connectToCluster(ctxName) // Підключення та перевірка версії

	stateMu.Lock()
	if err == nil {
		// Успішне підключення
		connectedContextName = ctxName
		currentClientset = clientset
		selectedResourceType = "" // НЕ вибираємо тип ресурсу за замовчуванням
		// Очищуємо ВСІ списки ресурсів при підключенні до нового кластера
		currentNodes = nil
		currentNamespaces = nil
		currentPods = nil
		currentDeployments = nil
		currentStatefulSets = nil
		currentDaemonSets = nil
		currentReplicaSets = nil
		currentJobs = nil
		currentCronJobs = nil
		currentConfigMaps = nil
		currentSecrets = nil
		currentServices = nil
		currentIngresses = nil
		logDebug("Збережено clientset: %s. Очікування вибору типу ресурсу.", ctxName)
		stateMu.Unlock()
		// НЕ запускаємо loadSelectedResources автоматично
		// Замість цього показуємо огляд кластера
		go displayClusterOverview(serverVersion) // Запускаємо показ огляду
		updateUIWidgets()                        // Оновлюємо UI (статус, виділення контексту/дерева)
	} else {
		// Помилка підключення
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
		currentConfigMaps = nil
		currentSecrets = nil
		currentServices = nil
		currentIngresses = nil
		logDebug("Помилка підключення.")
		stateMu.Unlock()
		displayEmptyOverview() // Показуємо порожню панель при помилці
		updateUIWidgets()      // Оновлюємо UI (статус помилки)
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
	if value == "" {
		value = "<none>"
	}
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
	// TODO: Додати більше полів - Conditions, Template...
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
func buildConfigMapDetailsView(cm corev1.ConfigMap) fyne.CanvasObject {
	logDebug("Створення деталей для ConfigMap: %s/%s", cm.Namespace, cm.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", cm.Name))
	detailsVBox.Add(createDetailRow("Namespace", cm.Namespace))
	detailsVBox.Add(createDetailRow("Created", cm.CreationTimestamp.Format(time.RFC1123)))
	detailsVBox.Add(createDetailRow("Data Keys", fmt.Sprintf("%d", len(cm.Data))))
	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildSecretDetailsView(secret corev1.Secret) fyne.CanvasObject {
	logDebug("Створення деталей для Secret: %s/%s", secret.Namespace, secret.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", secret.Name))
	detailsVBox.Add(createDetailRow("Namespace", secret.Namespace))
	detailsVBox.Add(createDetailRow("Created", secret.CreationTimestamp.Format(time.RFC1123)))
	detailsVBox.Add(createDetailRow("Type", string(secret.Type)))
	detailsVBox.Add(createDetailRow("Data Keys", fmt.Sprintf("%d", len(secret.Data))))
	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildServiceDetailsView(svc corev1.Service) fyne.CanvasObject {
	logDebug("Створення деталей для Service: %s/%s", svc.Namespace, svc.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", svc.Name))
	detailsVBox.Add(createDetailRow("Namespace", svc.Namespace))
	detailsVBox.Add(createDetailRow("Created", svc.CreationTimestamp.Format(time.RFC1123)))
	detailsVBox.Add(createDetailRow("Type", string(svc.Spec.Type)))
	detailsVBox.Add(createDetailRow("Cluster IP(s)", strings.Join(svc.Spec.ClusterIPs, ", ")))
	// External IPs (from LoadBalancer status)
	externalIPs := []string{}
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		if ingress.IP != "" {
			externalIPs = append(externalIPs, ingress.IP)
		}
		if ingress.Hostname != "" {
			externalIPs = append(externalIPs, ingress.Hostname)
		}
	}
	detailsVBox.Add(createDetailRow("External IP(s)", strings.Join(externalIPs, ", ")))
	// Ports
	portsStr := []string{}
	for _, port := range svc.Spec.Ports {
		pStr := fmt.Sprintf("%d", port.Port)
		if port.NodePort > 0 {
			pStr += fmt.Sprintf(":%d", port.NodePort)
		}
		pStr += "/" + string(port.Protocol)
		if port.Name != "" {
			pStr += " (" + port.Name + ")"
		}
		portsStr = append(portsStr, pStr)
	}
	detailsVBox.Add(createDetailRow("Ports", strings.Join(portsStr, ", ")))
	// Selector
	selectorStr := ""
	if len(svc.Spec.Selector) > 0 {
		parts := []string{}
		for k, v := range svc.Spec.Selector {
			parts = append(parts, fmt.Sprintf("%s=%s", k, v))
		}
		sort.Strings(parts)
		selectorStr = strings.Join(parts, ",")
	}
	detailsVBox.Add(createDetailRow("Selector", selectorStr))

	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildIngressDetailsView(ing networkingv1.Ingress) fyne.CanvasObject {
	logDebug("Створення деталей для Ingress: %s/%s", ing.Namespace, ing.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", ing.Name))
	detailsVBox.Add(createDetailRow("Namespace", ing.Namespace))
	detailsVBox.Add(createDetailRow("Created", ing.CreationTimestamp.Format(time.RFC1123)))
	className := "<default>"
	if ing.Spec.IngressClassName != nil {
		className = *ing.Spec.IngressClassName
	}
	detailsVBox.Add(createDetailRow("Class Name", className))

	// Rules
	detailsVBox.Add(widget.NewSeparator())
	detailsVBox.Add(widget.NewLabelWithStyle("Rules:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))
	rulesBox := container.NewVBox()
	if len(ing.Spec.Rules) == 0 {
		rulesBox.Add(widget.NewLabel("  <none>"))
	}
	for _, rule := range ing.Spec.Rules {
		host := rule.Host
		if host == "" {
			host = "*"
		}
		ruleStr := fmt.Sprintf(" - Host: %s", host)
		if rule.HTTP != nil {
			for _, path := range rule.HTTP.Paths {
				pathType := "Prefix"
				if path.PathType != nil {
					pathType = string(*path.PathType)
				}
				ruleStr += fmt.Sprintf("\n   Path: %s (%s) -> %s:%d", path.Path, pathType, path.Backend.Service.Name, path.Backend.Service.Port.Number)
			}
		}
		ruleLabel := widget.NewLabel(ruleStr)
		ruleLabel.Wrapping = fyne.TextWrapWord
		rulesBox.Add(ruleLabel)
	}
	detailsVBox.Add(rulesBox)

	// TLS
	if len(ing.Spec.TLS) > 0 {
		detailsVBox.Add(widget.NewSeparator())
		detailsVBox.Add(widget.NewLabelWithStyle("TLS:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))
		tlsBox := container.NewVBox()
		for _, tls := range ing.Spec.TLS {
			tlsStr := fmt.Sprintf(" - Secret: %s, Hosts: %s", tls.SecretName, strings.Join(tls.Hosts, ", "))
			tlsLabel := widget.NewLabel(tlsStr)
			tlsLabel.Wrapping = fyne.TextWrapWord
			tlsBox.Add(tlsLabel)
		}
		detailsVBox.Add(tlsBox)
	}

	// Status (LoadBalancer)
	detailsVBox.Add(widget.NewSeparator())
	detailsVBox.Add(widget.NewLabelWithStyle("LoadBalancer Status:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))
	lbStatus := []string{}
	for _, ingressStatus := range ing.Status.LoadBalancer.Ingress {
		if ingressStatus.IP != "" {
			lbStatus = append(lbStatus, ingressStatus.IP)
		}
		if ingressStatus.Hostname != "" {
			lbStatus = append(lbStatus, ingressStatus.Hostname)
		}
	}
	detailsVBox.Add(widget.NewLabel(strings.Join(lbStatus, ", ")))

	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildServiceAccountDetailsView(sa corev1.ServiceAccount) fyne.CanvasObject {
	logDebug("Створення деталей для ServiceAccount: %s/%s", sa.Namespace, sa.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", sa.Name))
	detailsVBox.Add(createDetailRow("Namespace", sa.Namespace))
	detailsVBox.Add(createDetailRow("Created", sa.CreationTimestamp.Format(time.RFC1123)))
	// TODO: Додати більше полів
	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildRoleDetailsView(role rbacv1.Role) fyne.CanvasObject {
	logDebug("Створення деталей для Role: %s/%s", role.Namespace, role.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", role.Name))
	detailsVBox.Add(createDetailRow("Namespace", role.Namespace))
	detailsVBox.Add(createDetailRow("Created", role.CreationTimestamp.Format(time.RFC1123)))
	// TODO: Додати більше полів (правила)
	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildRoleBindingDetailsView(rb rbacv1.RoleBinding) fyne.CanvasObject {
	logDebug("Створення деталей для RoleBinding: %s/%s", rb.Namespace, rb.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", rb.Name))
	detailsVBox.Add(createDetailRow("Namespace", rb.Namespace))
	detailsVBox.Add(createDetailRow("Created", rb.CreationTimestamp.Format(time.RFC1123)))
	detailsVBox.Add(createDetailRow("Role Kind", rb.RoleRef.Kind))
	detailsVBox.Add(createDetailRow("Role Name", rb.RoleRef.Name))
	// TODO: Додати більше полів (суб'єкти)
	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildClusterRoleDetailsView(cr rbacv1.ClusterRole) fyne.CanvasObject {
	logDebug("Створення деталей для ClusterRole: %s", cr.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", cr.Name))
	detailsVBox.Add(createDetailRow("Created", cr.CreationTimestamp.Format(time.RFC1123)))
	// TODO: Додати більше полів (правила)
	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildClusterRoleBindingDetailsView(crb rbacv1.ClusterRoleBinding) fyne.CanvasObject {
	logDebug("Створення деталей для ClusterRoleBinding: %s", crb.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", crb.Name))
	detailsVBox.Add(createDetailRow("Created", crb.CreationTimestamp.Format(time.RFC1123)))
	detailsVBox.Add(createDetailRow("Role Kind", crb.RoleRef.Kind))
	detailsVBox.Add(createDetailRow("Role Name", crb.RoleRef.Name))
	// TODO: Додати більше полів (суб'єкти)
	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildPersistentVolumeDetailsView(pv corev1.PersistentVolume) fyne.CanvasObject {
	logDebug("Створення деталей для PersistentVolume: %s", pv.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", pv.Name))
	detailsVBox.Add(createDetailRow("Created", pv.CreationTimestamp.Format(time.RFC1123)))
	detailsVBox.Add(createDetailRow("Capacity", pv.Spec.Capacity.Storage().String()))
	accessModes := make([]string, len(pv.Spec.AccessModes))
	for i, mode := range pv.Spec.AccessModes {
		accessModes[i] = string(mode)
	}
	detailsVBox.Add(createDetailRow("Access Modes", strings.Join(accessModes, ", ")))
	detailsVBox.Add(createDetailRow("Reclaim Policy", string(pv.Spec.PersistentVolumeReclaimPolicy)))
	if pv.Spec.StorageClassName != "" {
		detailsVBox.Add(createDetailRow("Storage Class", pv.Spec.StorageClassName))
	} else {
		detailsVBox.Add(createDetailRow("Storage Class", "<none>"))
	}
	if pv.Spec.ClaimRef != nil {
		detailsVBox.Add(createDetailRow("Claim", fmt.Sprintf("%s/%s", pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name)))
	} else {
		detailsVBox.Add(createDetailRow("Claim", "<none>"))
	}
	// TODO: Додати більше полів
	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildPersistentVolumeClaimDetailsView(pvc corev1.PersistentVolumeClaim) fyne.CanvasObject {
	logDebug("Створення деталей для PersistentVolumeClaim: %s/%s", pvc.Namespace, pvc.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", pvc.Name))
	detailsVBox.Add(createDetailRow("Namespace", pvc.Namespace))
	detailsVBox.Add(createDetailRow("Created", pvc.CreationTimestamp.Format(time.RFC1123)))
	detailsVBox.Add(createDetailRow("Status", string(pvc.Status.Phase)))
	detailsVBox.Add(createDetailRow("Capacity", pvc.Status.Capacity.Storage().String()))
	accessModes := make([]string, len(pvc.Status.AccessModes))
	for i, mode := range pvc.Status.AccessModes {
		accessModes[i] = string(mode)
	}
	detailsVBox.Add(createDetailRow("Access Modes", strings.Join(accessModes, ", ")))
	if pvc.Spec.StorageClassName != nil {
		detailsVBox.Add(createDetailRow("Storage Class", *pvc.Spec.StorageClassName))
	} else {
		detailsVBox.Add(createDetailRow("Storage Class", "<none>"))
	}
	if pvc.Spec.VolumeName != "" {
		detailsVBox.Add(createDetailRow("Volume", pvc.Spec.VolumeName))
	} else {
		detailsVBox.Add(createDetailRow("Volume", "<none>"))
	}
	// TODO: Додати більше полів
	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildStorageClassDetailsView(sc storagev1.StorageClass) fyne.CanvasObject {
	logDebug("Створення деталей для StorageClass: %s", sc.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", sc.Name))
	detailsVBox.Add(createDetailRow("Provisioner", sc.Provisioner))
	detailsVBox.Add(createDetailRow("Reclaim Policy", string(*sc.ReclaimPolicy)))
	if len(sc.MountOptions) > 0 {
		detailsVBox.Add(createDetailRow("Mount Options", strings.Join(sc.MountOptions, ", ")))
	} else {
		detailsVBox.Add(createDetailRow("Mount Options", "<none>"))
	}
	bindingMode := string(*sc.VolumeBindingMode)
	detailsVBox.Add(createDetailRow("Volume Binding Mode", bindingMode))
	// TODO: Додати більше полів (параметри)
	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildNotImplementedDetailsView(resourceType, resourceName string) fyne.CanvasObject {
	logDebug("Створення заглушки для деталей: %s %s", resourceType, resourceName)
	detailsVBox := container.NewVBox()
	label := widget.NewLabel(fmt.Sprintf("Детальний вигляд для типу '%s' ('%s') ще не реалізовано.", resourceType, resourceName))
	label.Wrapping = fyne.TextWrapWord
	label.Alignment = fyne.TextAlignCenter
	detailsVBox.Add(label)
	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewPadded(detailsVBox))
}
func buildErrorDetailsView(resourceType, resourceName string, err error) fyne.CanvasObject {
	detailsVBox := container.NewVBox()
	label := widget.NewLabel(fmt.Sprintf("Помилка завантаження деталей для %s '%s':\n%v", resourceType, resourceName, err))
	label.Wrapping = fyne.TextWrapWord
	label.Alignment = fyne.TextAlignCenter
	detailsVBox.Add(label)
	backButton := widget.NewButton(labelBackToList, func() { displayResourceList() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewPadded(detailsVBox))
}
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
	quitItem := fyne.NewMenuItem("Вийти", func() {
		logInfo("Клік 'Вийти'")
		stopFileWatcher()
		fyneApp.Quit()
	})

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
			name := ctxName // Захоплення змінної для замикання
			label := getDisplayName(name)
			targetSelection := connCtx
			if targetSelection == "" {
				targetSelection = currentCtx
			} // Позначаємо підключений або поточний з файлу
			if name == targetSelection {
				label = "✓ " + label
			} else {
				label = "  " + label
			}

			var action func()
			// Додаємо дію тільки якщо це НЕ поточний контекст з файлу
			// (щоб уникнути зайвих записів у файл)
			// АБО якщо ми хочемо завжди мати можливість "підключитися" до поточного
			// Давайте дозволимо клік на будь-який, крім вже ПІДКЛЮЧЕНОГО
			// if name != currentCtx { // Попередня логіка
			if name != connCtx { // Нова логіка: дозволяємо клік, якщо ще не підключені до цього контексту
				action = func() {
					logInfo("Вибрано контекст '%s' у меню трея/контекстному меню", name)
					// Запускаємо зміну default context у файлі та оновлення стану/UI
					go handleTrayContextSelection(name) // Викликаємо нову функцію
				}
			} else {
				action = nil // Немає дії для вже активного/підключеного
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

// Нова функція для обробки вибору контексту з меню (трея або вікна)
func handleTrayContextSelection(ctxName string) {
	logInfo("Спроба встановити '%s' як default context та оновити стан...", ctxName)
	// Спочатку змінюємо файл конфігурації
	err := switchContext(ctxName)
	if err != nil {
		logError("Не вдалося змінити default context на '%s': %v", ctxName, err)
		// TODO: Показати сповіщення користувачу про помилку?
		if statusBar != nil {
			statusBar.SetText(fmt.Sprintf("Помилка зміни default context: %v", err))
		}
		return // Не продовжуємо, якщо змінити файл не вдалося
	}
	// Якщо файл успішно змінено, запускаємо повне оновлення стану
	// loadAndUpdateState прочитає новий current-context з файлу
	loadAndUpdateState()
	// Після loadAndUpdateState UI оновить мітку поточного контексту
	// та список, а також скине активне підключення.
	// Користувач тепер має вибрати тип ресурсу для завантаження.
}
func showWindowContextMenu(pos fyne.Position) {
	menu := buildContextMenu()
	if mainWindow != nil {
		widget.ShowPopUpMenuAtPosition(menu, mainWindow.Canvas(), pos)
	}
}

// Оновлює меню в треї (якщо трей підтримується)
func updateSystemTrayMenu() {
	if desktopApp != nil { // Перевіряємо, чи трей було успішно ініціалізовано
		logDebug("Оновлення меню системного трея...")
		trayMenu = buildContextMenu() // Перебудовуємо меню на основі поточного стану
		desktopApp.SetSystemTrayMenu(trayMenu)
	}
}

// --- Моніторинг файлу ---
func setupFileWatcher() {
	stateMu.RLock()
	cfgFile := kubeconfigFile
	cfgEnvSet := isKubeconfigEnvSet
	stateMu.RUnlock()

	// Не запускаємо моніторинг, якщо використовується змінна KUBECONFIG
	if cfgEnvSet {
		logInfo("KUBECONFIG встановлено. Моніторинг файлу вимкнено.")
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

	// Додаємо директорію для моніторингу
	// Важливо моніторити саме директорію, бо багато редакторів/інструментів
	// зберігають файл через тимчасовий файл + перейменування.
	err = watcher.Add(watchDir)
	if err != nil {
		logError("Не вдалося додати шлях '%s' до file watcher: %v", watchDir, err)
		watcher.Close()
		watcher = nil
		return
	}

	logInfo("Запущено моніторинг файлу: %s (відстеження змін у %s)", watchDir, filepath.Base(cfgFile))
	watcherDone = make(chan bool)

	// Запускаємо горутину для обробки подій
	go func() {
		debounceTimer := time.NewTimer(time.Hour)
		debounceTimer.Stop() // Неактивний спочатку
		const debounceDuration = 750 * time.Millisecond

		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				} // Канал закрито
				logDebug("Подія watcher: Name: %s, Op: %s", event.Name, event.Op)
				stateMu.RLock()
				currentCfgFile := kubeconfigFile
				stateMu.RUnlock()
				// Реагуємо тільки на зміни нашого конфіг файлу
				if filepath.Clean(event.Name) == filepath.Clean(currentCfgFile) {
					if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) || event.Has(fsnotify.Remove) {
						logInfo("Виявлено зміну у файлі kubeconfig (%s). Перезапуск debounce...", event.Op)
						debounceTimer.Reset(debounceDuration)
					}
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				} // Канал закрито
				logError("Помилка watcher: %v", err)
			case <-debounceTimer.C:
				logInfo("Debounce timer. Оновлення стану через зміну файлу...")
				// Викликаємо повне оновлення стану, яке перечитає файл
				loadAndUpdateState()
			case <-watcherDone:
				logInfo("Зупинка горутини file watcher.")
				watcher.Close() // Закриваємо сам watcher
				debounceTimer.Stop()
				return
			}
		}
	}()
}
func stopFileWatcher() {
	if watcher != nil && watcherDone != nil {
		logInfo("Зупинка file watcher...")
		// Перевіряємо чи канал вже не закритий перед закриттям
		select {
		case <-watcherDone:
			// Вже закрито
		default:
			close(watcherDone) // Сигнал горутині зупинитися
		}
		watcher = nil
		watcherDone = nil
		logInfo("File watcher зупинено.")
	}
}

// --- Універсальна функція-диспетчер для показу деталей ---
func displayResourceDetails(resType string, resource interface{}) {
	var detailWidget fyne.CanvasObject
	resourceName := "?" // Ім'я за замовчуванням для заглушки

	// Використовуємо type assertion для виклику відповідної функції build...
	switch data := resource.(type) {
	case corev1.Namespace:
		detailWidget = buildNamespaceDetailsView(data)
		resourceName = data.Name
	case corev1.Node:
		detailWidget = buildNodeDetailsView(data)
		resourceName = data.Name
	case corev1.Pod:
		detailWidget = buildPodDetailsView(data)
		resourceName = data.Name
	case appsv1.Deployment:
		detailWidget = buildDeploymentDetailsView(data)
		resourceName = data.Name
	case appsv1.StatefulSet:
		detailWidget = buildStatefulSetDetailsView(data)
		resourceName = data.Name
	case appsv1.DaemonSet:
		detailWidget = buildDaemonSetDetailsView(data)
		resourceName = data.Name
	case appsv1.ReplicaSet:
		detailWidget = buildReplicaSetDetailsView(data)
		resourceName = data.Name
	case batchv1.Job:
		detailWidget = buildJobDetailsView(data)
		resourceName = data.Name
	case batchv1.CronJob:
		detailWidget = buildCronJobDetailsView(data)
		resourceName = data.Name
	case corev1.ConfigMap:
		detailWidget = buildConfigMapDetailsView(data)
		resourceName = data.Name
	case corev1.Secret:
		detailWidget = buildSecretDetailsView(data)
		resourceName = data.Name
	case corev1.Service:
		detailWidget = buildServiceDetailsView(data)
		resourceName = data.Name
	case networkingv1.Ingress:
		detailWidget = buildIngressDetailsView(data)
		resourceName = data.Name
	case corev1.ServiceAccount:
		detailWidget = buildServiceAccountDetailsView(data)
		resourceName = data.Name
	case rbacv1.Role:
		detailWidget = buildRoleDetailsView(data)
		resourceName = data.Name
	case rbacv1.RoleBinding:
		detailWidget = buildRoleBindingDetailsView(data)
		resourceName = data.Name
	case rbacv1.ClusterRole:
		detailWidget = buildClusterRoleDetailsView(data)
		resourceName = data.Name
	case rbacv1.ClusterRoleBinding:
		detailWidget = buildClusterRoleBindingDetailsView(data)
		resourceName = data.Name
	case corev1.PersistentVolume:
		detailWidget = buildPersistentVolumeDetailsView(data)
		resourceName = data.Name
	case corev1.PersistentVolumeClaim:
		detailWidget = buildPersistentVolumeClaimDetailsView(data)
		resourceName = data.Name
	case storagev1.StorageClass:
		detailWidget = buildStorageClassDetailsView(data)
		resourceName = data.Name
	default:
		logWarning("Немає функції деталей для типу %T", resource)
		detailWidget = buildNotImplementedDetailsView(resType, resourceName)
	}

	// Оновлюємо праву панель
	if rightPanelContainer != nil && detailWidget != nil {
		logDebug("Перемикання правої панелі на деталі для %s", resourceName)
		rightPanelContainer.Objects = []fyne.CanvasObject{detailWidget}
		rightPanelContainer.Refresh()
	} else {
		logError("Помилка при оновленні правої панелі для деталей %s", resourceName)
	}
}

// Показує огляд кластера (поки що лише версію)
func displayClusterOverview(serverVersion string) {
	logDebug("Показ огляду кластера")
	if rightPanelContainer == nil {
		logError("rightPanelContainer є nil при показі огляду")
		return
	}

	detailsVBox := container.NewVBox(
		widget.NewLabelWithStyle("Cluster Overview", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
		widget.NewSeparator(),
	)
	// Додаємо рядок з версією
	detailsVBox.Add(createDetailRow("Server Version", serverVersion))
	// TODO: В майбутньому можна додати сюди запити кількості Nodes, Namespaces тощо

	// Використовуємо Padded контейнер для кращого вигляду
	overviewContent := container.NewPadded(detailsVBox)

	rightPanelContainer.Objects = []fyne.CanvasObject{overviewContent}
	rightPanelContainer.Refresh()

	// Оновлюємо статус бар (повідомлення про підключення вже встановлено в connectToCluster)
	// Можна додати інструкцію
	stateMu.RLock()
	ctxListLen := len(allContextNames)
	connCtx := connectedContextName
	stateMu.RUnlock()
	if statusBar != nil {
		statusBar.SetText(fmt.Sprintf("Підключено: %s | Контекстів: %d | Виберіть тип ресурсу", getDisplayName(connCtx), ctxListLen))
	}

}

// --- Головна функція та запуск Fyne ---
func main() {
	log.SetFlags(log.Ldate | log.Ltime)
	logInfo("Запуск " + logPrefix + "...")
	logInfo("Версія Go: %s", runtime.Version())

	initializeLoadingRules()
	setupFileWatcher()
	fyneApp = app.New()

	// --- Повертаємо налаштування трея ---
	resIconPng := fyne.NewStaticResource("icon.png", iconData)
	// Перевірка, чи дані іконки завантажились (чи існує icon.png)
	if len(iconData) == 0 {
		logWarning("Дані іконки для трея порожні! Перевірте наявність icon.png та директиву //go:embed.")
		resIconPng = nil // Якщо даних немає, іконку не встановлюємо
	}

	// Перевіряємо підтримку системного трея та налаштовуємо його
	if drv, ok := fyneApp.(desktop.App); ok {
		desktopApp = drv // Зберігаємо для оновлення меню
		if resIconPng != nil {
			// Встановлюємо іконку, якщо вона успішно завантажена
			desktopApp.SetSystemTrayIcon(resIconPng)
		} else {
			logWarning("Не вдалося встановити іконку трея: ресурс порожній.")
			// Можна встановити якусь стандартну іконку Fyne як запасний варіант, якщо потрібно
			// desktopApp.SetSystemTrayIcon(theme.QuestionIcon())
		}
		// Створюємо та встановлюємо початкове меню трея
		trayMenu = buildContextMenu() // buildContextMenu має бути визначена
		desktopApp.SetSystemTrayMenu(trayMenu)
		logInfo("Системний трей налаштовано.")
	} else {
		desktopApp = nil // Явно вказуємо, що трей не підтримується
		logInfo("Системний трей не підтримується цією системою/драйвером.")
	}
	// -------------------------------------------

	mainWindow = fyneApp.NewWindow(appTitle)
	selectedResourceType = "Nodes"

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
			case "ConfigMaps":
				return len(currentConfigMaps)
			case "Secrets":
				return len(currentSecrets)
			case "Services":
				return len(currentServices)
			case "Ingresses":
				return len(currentIngresses)
			case "ServiceAccounts":
				return len(currentServiceAccounts)
			case "Roles":
				return len(currentRoles)
			case "RoleBindings":
				return len(currentRoleBindings)
			case "ClusterRoles":
				return len(currentClusterRoles)
			case "ClusterRoleBindings":
				return len(currentClusterRoleBindings)
			case "PersistentVolumes":
				return len(currentPersistentVolumes)
			case "PersistentVolumeClaims":
				return len(currentPersistentVolumeClaims)
			case "StorageClasses":
				return len(currentStorageClasses)
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
			case "ConfigMaps":
				if id >= 0 && id < len(currentConfigMaps) {
					name = fmt.Sprintf("%s/%s", currentConfigMaps[id].Namespace, currentConfigMaps[id].Name)
				}
			case "Secrets":
				if id >= 0 && id < len(currentSecrets) {
					name = fmt.Sprintf("%s/%s", currentSecrets[id].Namespace, currentSecrets[id].Name)
				}
			case "Services":
				if id >= 0 && id < len(currentServices) {
					name = fmt.Sprintf("%s/%s", currentServices[id].Namespace, currentServices[id].Name)
				}
			case "Ingresses":
				if id >= 0 && id < len(currentIngresses) {
					name = fmt.Sprintf("%s/%s", currentIngresses[id].Namespace, currentIngresses[id].Name)
				}
			case "ServiceAccounts":
				if id >= 0 && id < len(currentServiceAccounts) {
					name = fmt.Sprintf("%s/%s", currentServiceAccounts[id].Namespace, currentServiceAccounts[id].Name)
					if currentServiceAccounts[id].Namespace == "" {
						name = currentServiceAccounts[id].Name
					}
				}
			case "Roles":
				if id >= 0 && id < len(currentRoles) {
					name = fmt.Sprintf("%s/%s", currentRoles[id].Namespace, currentRoles[id].Name)
					if currentRoles[id].Namespace == "" {
						name = currentRoles[id].Name
					}
				}
			case "RoleBindings":
				if id >= 0 && id < len(currentRoleBindings) {
					name = fmt.Sprintf("%s/%s", currentRoleBindings[id].Namespace, currentRoleBindings[id].Name)
				}
			case "ClusterRoles":
				if id >= 0 && id < len(currentClusterRoles) {
					name = currentClusterRoles[id].Name
				}
			case "ClusterRoleBindings":
				if id >= 0 && id < len(currentClusterRoleBindings) {
					name = currentClusterRoleBindings[id].Name
				}
			case "PersistentVolumes":
				if id >= 0 && id < len(currentPersistentVolumes) {
					name = currentPersistentVolumes[id].Name
				}
			case "PersistentVolumeClaims":
				if id >= 0 && id < len(currentPersistentVolumeClaims) {
					name = fmt.Sprintf("%s/%s", currentPersistentVolumeClaims[id].Namespace, currentPersistentVolumeClaims[id].Name)
				}
			case "StorageClasses":
				if id >= 0 && id < len(currentStorageClasses) {
					name = currentStorageClasses[id].Name
				}
			}
			stateMu.RUnlock()
			item.(*widget.Label).SetText(name)
		},
	)
	resourceListWidget.OnSelected = func(id widget.ListItemID) {
		stateMu.RLock()
		resType := selectedResourceType
		var obj interface{}     // Узагальнений об'єкт
		var resourceName string // Ім'я для заглушки/логування
		// Отримуємо повний об'єкт зі зрізу
		switch resType {
		case "Namespaces":
			if id >= 0 && id < len(currentNamespaces) {
				obj = currentNamespaces[id]
				resourceName = currentNamespaces[id].Name
			}
		case "Nodes":
			if id >= 0 && id < len(currentNodes) {
				obj = currentNodes[id]
				resourceName = currentNodes[id].Name
			}
		case "Pods":
			if id >= 0 && id < len(currentPods) {
				obj = currentPods[id]
				resourceName = currentPods[id].Name
			}
		case "Deployments":
			if id >= 0 && id < len(currentDeployments) {
				obj = currentDeployments[id]
				resourceName = currentDeployments[id].Name
			}
		case "StatefulSets":
			if id >= 0 && id < len(currentStatefulSets) {
				obj = currentStatefulSets[id]
				resourceName = currentStatefulSets[id].Name
			}
		case "DaemonSets":
			if id >= 0 && id < len(currentDaemonSets) {
				obj = currentDaemonSets[id]
				resourceName = currentDaemonSets[id].Name
			}
		case "ReplicaSets":
			if id >= 0 && id < len(currentReplicaSets) {
				obj = currentReplicaSets[id]
				resourceName = currentReplicaSets[id].Name
			}
		case "Jobs":
			if id >= 0 && id < len(currentJobs) {
				obj = currentJobs[id]
				resourceName = currentJobs[id].Name
			}
		case "CronJobs":
			if id >= 0 && id < len(currentCronJobs) {
				obj = currentCronJobs[id]
				resourceName = currentCronJobs[id].Name
			}
		case "ConfigMaps":
			if id >= 0 && id < len(currentConfigMaps) {
				obj = currentConfigMaps[id]
				resourceName = currentConfigMaps[id].Name
			}
		case "Secrets":
			if id >= 0 && id < len(currentSecrets) {
				obj = currentSecrets[id]
				resourceName = currentSecrets[id].Name
			}
		case "Services":
			if id >= 0 && id < len(currentServices) {
				obj = currentServices[id]
				resourceName = currentServices[id].Name
			}
		case "Ingresses":
			if id >= 0 && id < len(currentIngresses) {
				obj = currentIngresses[id]
				resourceName = currentIngresses[id].Name
			}
		case "ServiceAccounts":
			if id >= 0 && id < len(currentServiceAccounts) {
				obj = currentServiceAccounts[id]
				resourceName = currentServiceAccounts[id].Name
			}
		case "Roles":
			if id >= 0 && id < len(currentRoles) {
				obj = currentRoles[id]
				resourceName = currentRoles[id].Name
			}
		case "RoleBindings":
			if id >= 0 && id < len(currentRoleBindings) {
				obj = currentRoleBindings[id]
				resourceName = currentRoleBindings[id].Name
			}
		case "ClusterRoles":
			if id >= 0 && id < len(currentClusterRoles) {
				obj = currentClusterRoles[id]
				resourceName = currentClusterRoles[id].Name
			}
		case "ClusterRoleBindings":
			if id >= 0 && id < len(currentClusterRoleBindings) {
				obj = currentClusterRoleBindings[id]
				resourceName = currentClusterRoleBindings[id].Name
			}
		case "PersistentVolumes":
			if id >= 0 && id < len(currentPersistentVolumes) {
				obj = currentPersistentVolumes[id]
				resourceName = currentPersistentVolumes[id].Name
			}
		case "PersistentVolumeClaims":
			if id >= 0 && id < len(currentPersistentVolumeClaims) {
				obj = currentPersistentVolumeClaims[id]
				resourceName = currentPersistentVolumeClaims[id].Name
			}
		case "StorageClasses":
			if id >= 0 && id < len(currentStorageClasses) {
				obj = currentStorageClasses[id]
				resourceName = currentStorageClasses[id].Name
			}
		default:
			logWarning("Вибрано ресурс невідомого типу '%s' для деталей", resType)
		}
		stateMu.RUnlock()

		if obj != nil {
			fullName := resourceName
			// Спробуємо отримати неймспейс, якщо він є у об'єкта (потрібно буде додати для інших типів при потребі)
			if nsGetter, ok := obj.(interface{ GetNamespace() string }); ok {
				if ns := nsGetter.GetNamespace(); ns != "" {
					fullName = ns + "/" + resourceName
				}
			}
			logInfo("Вибрано ресурс '%s': %s", resType, fullName)
			if statusBar != nil {
				statusBar.SetText(fmt.Sprintf("Вибрано %s: %s", resType, fullName))
			}
			// Викликаємо універсальну функцію показу деталей
			displayResourceDetails(resType, obj)
		} else {
			logWarning("Не вдалося отримати об'єкт для вибраного ресурсу типу '%s', ID: %d", resType, id)
		}
	}

	// --- Збираємо макет вікна ---
	leftPanelContent := container.NewVSplit(container.NewBorder(container.NewPadded(widget.NewLabel("Контексти:")), nil, nil, nil, contextListWidget), container.NewBorder(container.NewPadded(widget.NewLabel("Ресурси:")), nil, nil, nil, resourceTypeTree))
	leftPanelContent.Offset = 0.5
	rightPanelContainer = container.NewStack(resourceListWidget)
	tappableRightPanel := &tappableContainer{content: rightPanelContainer}
	tappableRightPanel.ExtendBaseWidget(tappableRightPanel)
	split := container.NewHSplit(leftPanelContent, tappableRightPanel)
	split.Offset = 0.3
	mainLayout := container.NewBorder(container.NewVBox(currentContextLabel, widget.NewSeparator()), statusBar, nil, nil, split)
	mainWindow.SetContent(mainLayout)

	mainWindow.Resize(fyne.NewSize(900, 700))
	mainWindow.CenterOnScreen()
	mainWindow.SetCloseIntercept(func() {
		logInfo("Закриття вікна...")
		stopFileWatcher() // <<<--- ЗУПИНЯЄМО МОНІТОРИНГ ПЕРЕД ВИХОДОМ
		fyneApp.Quit()
	})

	go loadAndUpdateState()
	mainWindow.ShowAndRun()
	logInfo(logPrefix + " завершено.")
}
