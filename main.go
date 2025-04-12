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
// const maxContextItems = 50
const labelLoading = "Завантаження..."
const labelError = "Помилка"
const labelNoContext = "Немає контексту"
const labelBackToList = "<- Назад до списку"
const appTitle = "Go Kube Manager (Lens Clone) - Step 8"
const appID = "goLENS-redacid"

var (
	fyneApp             fyne.App
	mainWindow          fyne.Window
	currentContextLabel *widget.Label
	contextListWidget   *widget.List
	resourceTypeTree    *widget.Tree
	//resourceListWidget  *widget.List
	resourceTable       *widget.Table
	resourceTableHeader fyne.CanvasObject
	rightPanelContainer *fyne.Container
	statusBar           *widget.Label
	desktopApp          desktop.App
	trayMenu            *fyne.Menu

	resourceTreeData = map[resourceTreeNodeID][]resourceTreeNodeID{
		"":               {"Cluster", "Workloads", "Network", "Storage", "Configuration", "Access Control"},
		"Cluster":        {"Namespaces", "Nodes", "System Workloads"}, // <<<--- ЗМІНЕНО/ДОДАНО
		"Workloads":      {"Pods", "Deployments", "StatefulSets", "DaemonSets", "ReplicaSets", "Jobs", "CronJobs"},
		"Network":        {"Services", "Ingresses"},
		"Storage":        {"PersistentVolumes", "PersistentVolumeClaims", "StorageClasses"},
		"Configuration":  {"ConfigMaps", "Secrets"},
		"Access Control": {"ServiceAccounts", "Roles", "RoleBindings", "ClusterRoles", "ClusterRoleBindings"},
	}
	resourceLeafNodes = map[resourceTreeNodeID]bool{"Namespaces": true, "Nodes": true, "Pods": true, "Deployments": true, "StatefulSets": true, "DaemonSets": true, "ReplicaSets": true, "Jobs": true, "CronJobs": true, "Services": true, "Ingresses": true, "PersistentVolumes": true, "PersistentVolumeClaims": true, "StorageClasses": true, "ConfigMaps": true, "Secrets": true, "ServiceAccounts": true, "Roles": true, "RoleBindings": true, "ClusterRoles": true, "ClusterRoleBindings": true, "System Workloads": true}
	arnRegex          = regexp.MustCompile(`^arn:aws:eks:[^:]+:(\d+):cluster/(.+)$`)

	currentSystemWorkloads []string
	currentContextName     string
	connectedContextName   string
	allContextNames        []string
	selectedResourceType   string
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
	watcherWg   sync.WaitGroup
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
		fyne.Do(func() {
			statusBar.SetText(fmt.Sprintf("Підключення до '%s'...", getDisplayName(contextName)))
		})
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
		fyne.Do(func() {
			statusBar.SetText(fmt.Sprintf("Підключено: %s (Сервер: %s)", getDisplayName(contextName), versionString))
		})
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

// Повертає заголовки колонок для таблиці залежно від типу ресурсу
func getHeadersForType(resType string) []string {
	switch resType {
	case "Namespaces":
		return []string{"Name", "Status", "Age"}
	case "Nodes":
		return []string{"Name", "Status", "Roles", "Version"}
	case "Pods":
		return []string{"Name", "Namespace", "Ready", "Restarts", "Controlled By", "Node", "QoS", "Age", "Status"}
	case "Deployments":
		return []string{"Name", "Namespace", "Ready", "Age"}
	case "StatefulSets":
		return []string{"Name", "Namespace", "Ready", "Age"}
	case "DaemonSets":
		return []string{"Name", "Namespace", "Desired", "Current", "Ready"} // Age видалено раніше
	case "ReplicaSets":
		return []string{"Name", "Namespace", "Desired", "Current", "Ready"}
	case "Jobs":
		return []string{"Name", "Namespace", "Completions", "Age"}
	case "CronJobs":
		return []string{"Name", "Namespace", "Schedule", "Suspend", "Last Schedule"}
	case "ConfigMaps":
		return []string{"Name", "Namespace", "Data Keys"}
	case "Secrets":
		return []string{"Name", "Namespace", "Type", "Data Keys"}
	case "Services":
		return []string{"Name", "Namespace", "Type", "ClusterIP", "Ports"}
	case "Ingresses":
		return []string{"Name", "Namespace", "Class", "Hosts"}
	case "System Workloads":
		return []string{"Component (in kube-system)"}
	default:
		return []string{"Name"} // Fallback
	}
}

// Форматує дані для конкретної клітинки таблиці
func formatCellData(resType string, row, col int) string {
	var dataStr string = ""
	validRow := false

	// Блокуємо читання даних
	stateMu.RLock()
	defer stateMu.RUnlock() // Гарантуємо розблокування

	// Перевірка індексу рядка та отримання даних (як було в UpdateCell)
	switch resType {
	case "Namespaces":
		validRow = row >= 0 && row < len(currentNamespaces)
		if validRow {
			ns := currentNamespaces[row]
			switch col {
			case 0:
				dataStr = ns.Name
			case 1:
				dataStr = string(ns.Status.Phase)
			case 2:
				dataStr = formatAge(ns.CreationTimestamp)
			}
		}
	case "Nodes":
		validRow = row >= 0 && row < len(currentNodes)
		if validRow {
			node := currentNodes[row]
			switch col {
			case 0:
				dataStr = node.Name
			case 1:
				dataStr = formatNodeStatus(node.Status.Conditions)
			case 2:
				dataStr = formatNodeRoles(node.Labels)
			case 3:
				dataStr = node.Status.NodeInfo.KubeletVersion
			}
		}
	case "Pods":
		validRow = row >= 0 && row < len(currentPods)
		if validRow {
			pod := currentPods[row]
			switch col {
			case 0:
				dataStr = pod.Name
			case 1:
				dataStr = pod.Namespace
			case 2:
				dataStr = formatPodContainers(pod.Status.ContainerStatuses)
			case 3:
				dataStr = formatPodRestarts(pod.Status.ContainerStatuses)
			case 4:
				dataStr = formatOwnerRefs(pod.OwnerReferences)
			case 5:
				dataStr = pod.Spec.NodeName
			case 6:
				dataStr = string(pod.Status.QOSClass)
			case 7:
				dataStr = formatAge(pod.CreationTimestamp)
			case 8:
				dataStr = string(pod.Status.Phase)
			}
		}
	case "Deployments":
		validRow = row >= 0 && row < len(currentDeployments)
		if validRow {
			dep := currentDeployments[row]
			switch col {
			case 0:
				dataStr = dep.Name
			case 1:
				dataStr = dep.Namespace
			case 2:
				dataStr = fmt.Sprintf("%d/%d", dep.Status.ReadyReplicas, *dep.Spec.Replicas)
			case 3:
				dataStr = formatAge(dep.CreationTimestamp)
			}
		}
	case "StatefulSets":
		validRow = row >= 0 && row < len(currentStatefulSets)
		if validRow {
			sts := currentStatefulSets[row]
			switch col {
			case 0:
				dataStr = sts.Name
			case 1:
				dataStr = sts.Namespace
			case 2:
				dataStr = fmt.Sprintf("%d/%d", sts.Status.ReadyReplicas, *sts.Spec.Replicas)
			case 3:
				dataStr = formatAge(sts.CreationTimestamp)
			}
		}
	case "DaemonSets":
		validRow = row >= 0 && row < len(currentDaemonSets)
		if validRow {
			ds := currentDaemonSets[row]
			switch col {
			case 0:
				dataStr = ds.Name
			case 1:
				dataStr = ds.Namespace
			case 2:
				dataStr = fmt.Sprintf("%d", ds.Status.DesiredNumberScheduled)
			case 3:
				dataStr = fmt.Sprintf("%d", ds.Status.CurrentNumberScheduled)
			case 4:
				dataStr = fmt.Sprintf("%d", ds.Status.NumberReady)
			}
		}
	case "ReplicaSets":
		validRow = row >= 0 && row < len(currentReplicaSets)
		if validRow {
			rs := currentReplicaSets[row]
			switch col {
			case 0:
				dataStr = rs.Name
			case 1:
				dataStr = rs.Namespace
			case 2:
				dataStr = fmt.Sprintf("%d", *rs.Spec.Replicas)
			case 3:
				dataStr = fmt.Sprintf("%d", rs.Status.Replicas)
			case 4:
				dataStr = fmt.Sprintf("%d", rs.Status.ReadyReplicas)
			}
		}
	case "Jobs":
		validRow = row >= 0 && row < len(currentJobs)
		if validRow {
			job := currentJobs[row]
			switch col {
			case 0:
				dataStr = job.Name
			case 1:
				dataStr = job.Namespace
			case 2:
				comp := "N/A"
				if job.Spec.Completions != nil {
					comp = fmt.Sprintf("%d/%d", job.Status.Succeeded, *job.Spec.Completions)
				} else {
					comp = fmt.Sprintf("%d/?", job.Status.Succeeded)
				}
				dataStr = comp
			case 3:
				dataStr = formatAge(job.CreationTimestamp)
			}
		}
	case "CronJobs":
		validRow = row >= 0 && row < len(currentCronJobs)
		if validRow {
			cj := currentCronJobs[row]
			switch col {
			case 0:
				dataStr = cj.Name
			case 1:
				dataStr = cj.Namespace
			case 2:
				dataStr = cj.Spec.Schedule
			case 3:
				susp := "False"
				if cj.Spec.Suspend != nil && *cj.Spec.Suspend {
					susp = "True"
				}
				dataStr = susp
			case 4:
				last := "Never"
				if cj.Status.LastScheduleTime != nil {
					last = formatAge(*cj.Status.LastScheduleTime)
				}
				dataStr = last
			}
		}
	case "ConfigMaps":
		validRow = row >= 0 && row < len(currentConfigMaps)
		if validRow {
			cm := currentConfigMaps[row]
			switch col {
			case 0:
				dataStr = cm.Name
			case 1:
				dataStr = cm.Namespace
			case 2:
				dataStr = fmt.Sprintf("%d", len(cm.Data))
			}
		}
	case "Secrets":
		validRow = row >= 0 && row < len(currentSecrets)
		if validRow {
			secret := currentSecrets[row]
			switch col {
			case 0:
				dataStr = secret.Name
			case 1:
				dataStr = secret.Namespace
			case 2:
				dataStr = string(secret.Type)
			case 3:
				dataStr = fmt.Sprintf("%d", len(secret.Data))
			}
		}
	case "Services":
		validRow = row >= 0 && row < len(currentServices)
		if validRow {
			svc := currentServices[row]
			switch col {
			case 0:
				dataStr = svc.Name
			case 1:
				dataStr = svc.Namespace
			case 2:
				dataStr = string(svc.Spec.Type)
			case 3:
				dataStr = strings.Join(svc.Spec.ClusterIPs, ",")
			case 4:
				dataStr = formatPorts(svc.Spec.Ports)
			}
		}
	case "Ingresses":
		validRow = row >= 0 && row < len(currentIngresses)
		if validRow {
			ing := currentIngresses[row]
			switch col {
			case 0:
				dataStr = ing.Name
			case 1:
				dataStr = ing.Namespace
			case 2:
				class := "<default>"
				if ing.Spec.IngressClassName != nil {
					class = *ing.Spec.IngressClassName
				}
				dataStr = class
			case 3:
				dataStr = formatIngressHosts(ing.Spec.Rules)
			}
		}
	case "System Workloads":
		validRow = row >= 0 && row < len(currentSystemWorkloads)
		if validRow {
			if col == 0 {
				dataStr = currentSystemWorkloads[row]
			}
		}
	}

	if !validRow {
		logDebug("Спроба форматувати недійсну клітинку: Row %d, Col %d (Type: %s)", row, col, resType)
	}

	return dataStr
}
func formatAge(t metav1.Time) string {
	d := time.Since(t.Time)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func formatPodContainers(statuses []corev1.ContainerStatus) string {
	ready := 0
	total := len(statuses)
	for _, cs := range statuses {
		if cs.Ready {
			ready++
		}
	}
	return fmt.Sprintf("%d/%d", ready, total)
}

func formatPodRestarts(statuses []corev1.ContainerStatus) string {
	restarts := int32(0)
	for _, cs := range statuses {
		restarts += cs.RestartCount
	}
	return fmt.Sprintf("%d", restarts)
}

func formatOwnerRefs(refs []metav1.OwnerReference) string {
	if len(refs) == 0 {
		return "<none>"
	}
	owners := make([]string, len(refs))
	for i, owner := range refs {
		owners[i] = fmt.Sprintf("%s/%s", owner.Kind, owner.Name)
	}
	// Показуємо тільки перший для стислості в таблиці? Або всі?
	// return owners[0]
	return strings.Join(owners, ",") // Покажемо всі через кому
}

func formatNodeStatus(conditions []corev1.NodeCondition) string {
	for _, cond := range conditions {
		if cond.Type == corev1.NodeReady {
			if cond.Status == corev1.ConditionTrue {
				return "Ready"
			}
			// Додамо інші статуси, якщо Ready=False або Unknown
			for _, c := range conditions {
				// Шукаємо причину неготовності (якщо є)
				if c.Status != corev1.ConditionTrue && c.Reason != "" {
					return c.Reason
				}
			}
			return "NotReady"
		}
	}
	return "Unknown"
}
func formatNodeRoles(labels map[string]string) string {
	roles := []string{}
	for label := range labels {
		if strings.HasPrefix(label, "node-role.kubernetes.io/") {
			roles = append(roles, strings.TrimPrefix(label, "node-role.kubernetes.io/"))
		}
	}
	if len(roles) == 0 {
		roles = append(roles, "<none>")
	}
	sort.Strings(roles)
	return strings.Join(roles, ",")
}
func formatPorts(ports []corev1.ServicePort) string {
	portsStr := []string{}
	for _, port := range ports {
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
	return strings.Join(portsStr, ", ")
}
func formatIngressHosts(rules []networkingv1.IngressRule) string {
	hosts := []string{}
	for _, rule := range rules {
		if rule.Host != "" {
			hosts = append(hosts, rule.Host)
		}
	}
	if len(hosts) == 0 {
		return "*"
	}
	return strings.Join(hosts, ",")
}

// Розраховує та встановлює ширину колонок таблиці на основі вмісту
// Розраховує ширину колонок на основі вмісту та заголовків
func calculateColumnWidths(resType string) []float32 {
	logDebug("Розрахунок ширини колонок для типу: %s", resType)

	stateMu.RLock()
	numRows, numCols := 0, 1
	switch resType {
	case "Namespaces":
		numRows = len(currentNamespaces)
		numCols = 3
	case "Nodes":
		numRows = len(currentNodes)
		numCols = 4
	case "Pods":
		numRows = len(currentPods)
		numCols = 9
	case "Deployments":
		numRows = len(currentDeployments)
		numCols = 4
	case "StatefulSets":
		numRows = len(currentStatefulSets)
		numCols = 4
	case "DaemonSets":
		numRows = len(currentDaemonSets)
		numCols = 5
	case "ReplicaSets":
		numRows = len(currentReplicaSets)
		numCols = 5
	case "Jobs":
		numRows = len(currentJobs)
		numCols = 4
	case "CronJobs":
		numRows = len(currentCronJobs)
		numCols = 5
	case "ConfigMaps":
		numRows = len(currentConfigMaps)
		numCols = 3
	case "Secrets":
		numRows = len(currentSecrets)
		numCols = 4
	case "Services":
		numRows = len(currentServices)
		numCols = 5
	case "Ingresses":
		numRows = len(currentIngresses)
		numCols = 4
	case "System Workloads":
		numRows = len(currentSystemWorkloads)
		numCols = 1
	}
	stateMu.RUnlock()

	if numRows == 0 || numCols == 0 {
		logDebug("Немає даних або колонок для розрахунку ширини.")
		// Повертаємо nil або зріз нулів, щоб SetColumnWidth не викликався
		return nil
	}

	headers := getHeadersForType(resType)
	if len(headers) != numCols {
		logError("Невідповідність заголовків (%d) та колонок (%d) для %s", len(headers), numCols, resType)
		// Повертаємо nil, щоб уникнути паніки
		return nil
	}

	maxWidths := make([]float32, numCols)
	padding := float32(20) // Збільшимо відступ

	for col := 0; col < numCols; col++ {
		headerWidth := fyne.MeasureText(headers[col], theme.TextSize(), fyne.TextStyle{Bold: true}).Width
		maxWidths[col] = headerWidth

		// Важливо! Розрахунок по всіх рядках може бути ДУЖЕ повільним.
		// Розгляньте обмеження кількості рядків для розрахунку (напр., перші 100)
		// Hoặc розрахунок лише по видимих рядках (складніше).
		maxRowsToCheck := 100 // Обмеження для продуктивності
		if numRows > maxRowsToCheck {
			numRows = maxRowsToCheck
		}

		for row := 0; row < numRows; row++ {
			cellData := formatCellData(resType, row, col) // Використовуємо хелпер
			// Використовуємо той самий стиль/розмір, що й у CreateCell
			cellWidth := fyne.MeasureText(cellData, theme.TextSize(), fyne.TextStyle{}).Width
			if cellWidth > maxWidths[col] {
				maxWidths[col] = cellWidth
			}
		}
		maxWidths[col] += padding // Додаємо відступ
		logDebug("Розрахована ширина колонки %d ('%s') = %.2f", col, headers[col], maxWidths[col])
	}
	return maxWidths
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
	sysWorkloadCount := len(currentSystemWorkloads)

	stateMu.RUnlock()

	displayCtxFromFile := labelNoContext
	if ctxFromFile != "" {
		displayCtxFromFile = getDisplayName(ctxFromFile)
	}
	if currentContextLabel != nil {
		fyne.Do(func() {
			currentContextLabel.SetText("Поточний у файлі: " + displayCtxFromFile)
		})
	}

	if contextListWidget != nil { /* ... оновлення списку контекстів ... */
		fyne.Do(func() {
			contextListWidget.Refresh()
		})
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
	if resourceTypeTree != nil {
		fyne.Do(func() {
			resourceTypeTree.Refresh()
		})
		if resType != "" {
			resourceTypeTree.Select(resType)
		} else {
			fyne.Do(func() {
				resourceTypeTree.UnselectAll()
			})
		}
	}

	resourceCount := 0
	if resourceTable != nil {
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
		case "System Workloads":
			resourceCount = sysWorkloadCount
		// Додайте інші типи тут...
		default:
			resourceCount = 0
		}
		stateMu.RUnlock()
		logDebug("Оновлення списку ресурсів '%s' у UI (%d елементів)", resType, resourceCount)
		fyne.Do(func() {
			resourceTable.Refresh()
		})
	}

	updateSystemTrayMenu()

	if statusBar != nil && !strings.HasPrefix(statusMsg, "Помилка") && !strings.HasPrefix(statusMsg, "Підключення") && !strings.HasPrefix(statusMsg, "Завантаження") {
		// Оновлено рядок стану
		fyne.Do(func() {
			statusBar.SetText(fmt.Sprintf("Контекстів: %d | %s: %d", len(ctxList), resType, resourceCount))
		})
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
	currentSystemWorkloads = nil
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

	displayResourceTableView()
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
		fyne.Do(func() {
			if statusBar != nil {
				statusBar.SetText("Не підключено.")
			}
			displayResourceTableView()
			updateUIWidgets()
		})
		return
	}
	logInfo("Завантаження ресурсів типу '%s' для '%s'", resType, contextName)
	if fyne.CurrentApp() != nil && statusBar != nil {
		fyne.Do(func() {
			statusBar.SetText(fmt.Sprintf("Завантаження %s для '%s'...", resType, getDisplayName(contextName)))
		})
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
	newConfigMaps := []corev1.ConfigMap{}
	newSecrets := []corev1.Secret{}
	newServices := []corev1.Service{}
	newIngresses := []networkingv1.Ingress{}
	newSystemWorkloads := []string{}

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
	currentConfigMaps = nil
	currentSecrets = nil
	currentServices = nil
	currentIngresses = nil
	currentSystemWorkloads = nil
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
	case "System Workloads":
		logInfo("Завантаження системних компонентів з kube-system...")
		workloadNamespace := "kube-system"
		var combinedErr error
		depList, depErr := clientset.AppsV1().Deployments(workloadNamespace).List(ctxTimeout, listOptions)
		if depErr != nil {
			logError("Помилка List Deployments (%s): %v", workloadNamespace, depErr)
			errors.Join(combinedErr, depErr)
		} else {
			for _, item := range depList.Items {
				newSystemWorkloads = append(newSystemWorkloads, fmt.Sprintf("deploy/%s", item.Name))
			}
		}
		dsList, dsErr := clientset.AppsV1().DaemonSets(workloadNamespace).List(ctxTimeout, listOptions)
		if dsErr != nil {
			logError("Помилка List DaemonSets (%s): %v", workloadNamespace, dsErr)
			errors.Join(combinedErr, dsErr)
		} else {
			for _, item := range dsList.Items {
				newSystemWorkloads = append(newSystemWorkloads, fmt.Sprintf("ds/%s", item.Name))
			}
		}
		stsList, stsErr := clientset.AppsV1().StatefulSets(workloadNamespace).List(ctxTimeout, listOptions)
		if stsErr != nil {
			logError("Помилка List StatefulSets (%s): %v", workloadNamespace, stsErr)
			errors.Join(combinedErr, stsErr)
		} else {
			for _, item := range stsList.Items {
				newSystemWorkloads = append(newSystemWorkloads, fmt.Sprintf("sts/%s", item.Name))
			}
		}
		sort.Strings(newSystemWorkloads)
		err = combinedErr
		if err != nil {
			statusMsg = fmt.Sprintf("Помилка завантаження компонентів: %v", err)
		}
	default:
		logWarning("Невідомий тип ресурсу: %s", resType)
		err = fmt.Errorf("тип %s не підтримується", resType)
	}

	if err != nil && statusMsg == "OK" {
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
	case "ConfigMaps":
		currentConfigMaps = newConfigMaps
	case "Secrets":
		currentSecrets = newSecrets
	case "Services":
		currentServices = newServices
	case "Ingresses":
		currentIngresses = newIngresses
	case "System Workloads":
		currentSystemWorkloads = newSystemWorkloads
	}
	stateMu.Unlock()

	var calculatedWidths []float32
	if err == nil {
		calculatedWidths = calculateColumnWidths(resType)
	}

	app := fyne.CurrentApp()
	if app != nil {
		fyne.Do(func() {
			updateTableLayout(resType, calculatedWidths)
			if statusBar != nil {
				finalStatusMsg := statusMsg
				if err == nil {
					count := 0
					stateMu.RLock()
					switch resType {
					case "Namespaces":
						count = len(currentNamespaces)
					case "Nodes":
						count = len(currentNodes)
					case "Pods":
						count = len(currentPods)
					case "Deployments":
						count = len(currentDeployments)
					case "StatefulSets":
						count = len(currentStatefulSets)
					case "DaemonSets":
						count = len(currentDaemonSets)
					case "ReplicaSets":
						count = len(currentReplicaSets)
					case "Jobs":
						count = len(currentJobs)
					case "CronJobs":
						count = len(currentCronJobs)
					case "ConfigMaps":
						count = len(currentConfigMaps)
					case "Secrets":
						count = len(currentSecrets)
					case "Services":
						count = len(currentServices)
					case "Ingresses":
						count = len(currentIngresses)
					case "System Workloads":
						count = len(currentSystemWorkloads)
					}
					stateMu.RUnlock()
					finalStatusMsg = fmt.Sprintf("Підключено: %s | %s: %d", getDisplayName(contextName), resType, count)
				}
				statusBar.SetText(finalStatusMsg)
			}
			updateUIWidgets() // Викликаємо загальне оновлення в кінці
		})
	}
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
		fyne.Do(func() {
			rightPanelContainer.Refresh()
		})
	} else {
		logError("rightPanelContainer є nil при показі порожньої панелі")
	}
	// Також оновлюємо resourceTable, щоб він був порожнім
	if resourceTable != nil {
		fyne.Do(func() {
			resourceTable.Refresh()
		})
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
		currentSystemWorkloads = nil
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

// Створює віджет заголовка таблиці з заданою шириною колонок
func buildTableHeader(resType string, colWidths []float32) fyne.CanvasObject {
	headers := getHeadersForType(resType)
	if len(headers) == 0 || len(colWidths) != len(headers) {
		// Повертаємо простий заголовок за замовчуванням або порожній контейнер
		logWarning("Не вдалося створити заголовок: невідповідність колонок/ширин для %s", resType)
		return container.NewHBox(widget.NewLabelWithStyle("Name", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})) // Default
	}

	headerWidgets := []fyne.CanvasObject{}
	for _, h := range headers {
		label := widget.NewLabelWithStyle(h, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
		// Встановлюємо мінімальну ширину для кожної мітки заголовка
		// label.min = fyne.NewSize(colWidths[i], 0) // MinSize - це не те, що нам треба
		// Ми не можемо напряму встановити ширину віджета в Grid.
		// Замість цього, будемо покладатися на те, що таблиця нижче
		// "розтягне" Grid за допомогою SetColumnWidth.
		// Але для HBox можна спробувати так:
		// hboxCell := container.NewPadded(label) // Додаємо відступи
		// hboxCell.Resize(fyne.NewSize(colWidths[i], hboxCell.MinSize().Height)) // Спроба встановити розмір - може не спрацювати ідеально з HBox
		headerWidgets = append(headerWidgets, label) // Додаємо мітку
	}

	// Використання HBox може не дати точного вирівнювання з колонками таблиці,
	// якщо сумарна ширина перевищує доступну. Grid - кращий варіант,
	// але SetColumnWidth налаштовує саму таблицю, а не зовнішній віджет.
	// Пробуємо повернути HBox з мітками - можливо, таблиця вплине на його рендеринг.
	return container.NewPadded(container.NewHBox(headerWidgets...))
}

// Оновлює ширину колонок таблиці та її заголовок
func updateTableLayout(resType string, colWidths []float32) {
	if resourceTable == nil {
		return
	}

	if colWidths != nil {
		// Встановлюємо розраховану ширину для колонок самої таблиці
		logDebug("Встановлення ширини колонок таблиці...")
		for col, width := range colWidths {
			resourceTable.SetColumnWidth(col, width)
		}
	} else {
		// Якщо ширини не розраховано (помилка або немає даних),
		// можна скинути ширину до стандартної або нічого не робити.
		// Скидання може викликати стрибок розміру.
		// Поки що нічого не робимо.
		logDebug("Ширини колонок не розраховано, SetColumnWidth не викликається.")
	}

	// Оновлюємо/перестворюємо віджет заголовка
	newHeader := buildTableHeader(resType, colWidths)
	resourceTableHeader = newHeader // Зберігаємо новий заголовок глобально

	// Оновлюємо контейнер правої панелі, щоб показати таблицю з (можливо) новим заголовком
	displayResourceTableView() // Ця функція тепер використає оновлений resourceTableHeader
}

// Показує таблицю ресурсів з поточним заголовком
func displayResourceTableView() {
	logDebug("Показ таблиці ресурсів")

	// Переконуємося, що глобальний заголовок існує
	if resourceTableHeader == nil {
		logWarning("Спроба показати таблицю, але resourceTableHeader є nil. Створюємо стандартний.")
		// Створюємо простий заголовок, якщо його немає
		stateMu.RLock()
		resType := selectedResourceType
		stateMu.RUnlock()
		resourceTableHeader = buildTableHeader(resType, nil) // nil для ширин означатиме стандартний
	}

	// Створюємо/оновлюємо контейнер для таблиці та її заголовка
	tableContainer := container.NewBorder(resourceTableHeader, nil, nil, nil, resourceTable)

	if rightPanelContainer != nil && resourceTable != nil {
		resourceTable.Refresh() // Оновлюємо дані таблиці
		// Встановлюємо tableContainer як вміст правої панелі
		if len(rightPanelContainer.Objects) == 0 || rightPanelContainer.Objects[0] != tableContainer {
			rightPanelContainer.Objects = []fyne.CanvasObject{tableContainer}
		} else {
			// Якщо це вже контейнер таблиці, оновлюємо його (змінився заголовок або таблиця)
			rightPanelContainer.Objects[0] = tableContainer
		}
		rightPanelContainer.Refresh()
	} else {
		logError("rightPanelContainer або resourceTable є nil при показі таблиці")
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
	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })
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
	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })
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
	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })
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
	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })
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
	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })
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
	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildReplicaSetDetailsView(rs appsv1.ReplicaSet) fyne.CanvasObject {
	logDebug("Створення деталей для ReplicaSet: %s/%s", rs.Namespace, rs.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", rs.Name))
	detailsVBox.Add(createDetailRow("Namespace", rs.Namespace))
	detailsVBox.Add(createDetailRow("Created", rs.CreationTimestamp.Format(time.RFC1123)))
	detailsVBox.Add(createDetailRow("Replicas", fmt.Sprintf("%d desired, %d current, %d ready", *rs.Spec.Replicas, rs.Status.Replicas, rs.Status.ReadyReplicas)))
	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })
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
	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })
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
	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildConfigMapDetailsView(cm corev1.ConfigMap) fyne.CanvasObject {
	logDebug("Створення деталей для ConfigMap: %s/%s", cm.Namespace, cm.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", cm.Name))
	detailsVBox.Add(createDetailRow("Namespace", cm.Namespace))
	detailsVBox.Add(createDetailRow("Created", cm.CreationTimestamp.Format(time.RFC1123)))
	detailsVBox.Add(createDetailRow("Data Keys", fmt.Sprintf("%d", len(cm.Data))))
	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })
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
	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })
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

	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })
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

	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildServiceAccountDetailsView(sa corev1.ServiceAccount) fyne.CanvasObject {
	logDebug("Створення деталей для ServiceAccount: %s/%s", sa.Namespace, sa.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", sa.Name))
	detailsVBox.Add(createDetailRow("Namespace", sa.Namespace))
	detailsVBox.Add(createDetailRow("Created", sa.CreationTimestamp.Format(time.RFC1123)))
	// TODO: Додати більше полів
	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildRoleDetailsView(role rbacv1.Role) fyne.CanvasObject {
	logDebug("Створення деталей для Role: %s/%s", role.Namespace, role.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", role.Name))
	detailsVBox.Add(createDetailRow("Namespace", role.Namespace))
	detailsVBox.Add(createDetailRow("Created", role.CreationTimestamp.Format(time.RFC1123)))
	// TODO: Додати більше полів (правила)
	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })
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
	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildClusterRoleDetailsView(cr rbacv1.ClusterRole) fyne.CanvasObject {
	logDebug("Створення деталей для ClusterRole: %s", cr.Name)
	detailsVBox := container.NewVBox()
	detailsVBox.Add(createDetailRow("Name", cr.Name))
	detailsVBox.Add(createDetailRow("Created", cr.CreationTimestamp.Format(time.RFC1123)))
	// TODO: Додати більше полів (правила)
	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })
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
	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })
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
	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })
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
	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })
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
	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewVScroll(detailsVBox))
}
func buildNotImplementedDetailsView(resourceType, resourceName string) fyne.CanvasObject {
	logDebug("Створення заглушки для деталей: %s %s", resourceType, resourceName)
	detailsVBox := container.NewVBox()
	label := widget.NewLabel(fmt.Sprintf("Детальний вигляд для типу '%s' ('%s') ще не реалізовано.", resourceType, resourceName))
	label.Wrapping = fyne.TextWrapWord
	label.Alignment = fyne.TextAlignCenter
	detailsVBox.Add(label)
	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewPadded(detailsVBox))
}

func buildErrorDetailsView(resourceType, resourceName string, err error) fyne.CanvasObject {
	detailsVBox := container.NewVBox()
	label := widget.NewLabel(fmt.Sprintf("Помилка завантаження деталей для %s '%s':\n%v", resourceType, resourceName, err))
	label.Wrapping = fyne.TextWrapWord
	label.Alignment = fyne.TextAlignCenter
	detailsVBox.Add(label)
	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })
	return container.NewBorder(backButton, nil, nil, nil, container.NewPadded(detailsVBox))
}

// Отримує повний об'єкт ресурсу System Workload за типом/іменем/неймспейсом і показує його деталі
func fetchAndDisplayResourceDetails(resTypePrefix string, namespace string, name string) {
	logInfo("Запит деталей для %s: %s/%s", resTypePrefix, namespace, name)

	// Показуємо індикатор завантаження безпечно
	queueUIUpdate(func() { // Використовуємо хелпер, якщо він є, або fyneApp.Do
		if rightPanelContainer != nil {
			loadingLabel := widget.NewLabel(fmt.Sprintf("Завантаження деталей для %s %s/%s...", resTypePrefix, namespace, name))
			loadingLabel.Alignment = fyne.TextAlignCenter
			rightPanelContainer.Objects = []fyne.CanvasObject{container.NewCenter(loadingLabel)}
			rightPanelContainer.Refresh()
		}
	})

	stateMu.RLock()
	clientset := currentClientset
	stateMu.RUnlock()

	if clientset == nil {
		logError("Неможливо отримати деталі: немає clientset")
		// Показуємо помилку через диспетчер деталей (передаємо помилку)
		queueUIUpdate(func() { // Використовуємо хелпер, якщо він є, або fyneApp.Do
			displayResourceDetails("Error", fmt.Errorf("немає активного підключення до кластера"))
		})
		return
	}

	var obj interface{} // Тут буде повний об'єкт
	var err error
	var finalResType string // Повний тип ресурсу для диспетчера деталей

	ctxTimeout, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Робимо Get запит залежно від префіксу типу
	switch resTypePrefix {
	case "deploy":
		finalResType = "Deployments"
		obj, err = clientset.AppsV1().Deployments(namespace).Get(ctxTimeout, name, metav1.GetOptions{})
	case "ds":
		finalResType = "DaemonSets"
		obj, err = clientset.AppsV1().DaemonSets(namespace).Get(ctxTimeout, name, metav1.GetOptions{})
	case "sts":
		finalResType = "StatefulSets"
		obj, err = clientset.AppsV1().StatefulSets(namespace).Get(ctxTimeout, name, metav1.GetOptions{})
	default:
		err = fmt.Errorf("невідомий префікс типу '%s' для System Workload", resTypePrefix)
	}

	// Викликаємо диспетчер деталей з результатом (об'єктом або помилкою)
	queueUIUpdate(func() { // Використовуємо хелпер, якщо він є, або fyneApp.Do
		if err != nil {
			logError("Помилка отримання деталей для %s %s/%s: %v", resTypePrefix, namespace, name, err)
			// Передаємо помилку в диспетчер, який викличе buildErrorDetailsView
			displayResourceDetails(finalResType+" ("+resTypePrefix+")", err) // Передаємо тип та помилку
		} else if obj != nil {
			logDebug("Успішно отримано деталі для %s %s/%s", resTypePrefix, namespace, name)
			displayResourceDetails(finalResType, obj) // Передаємо тип та об'єкт
		} else {
			// Цього не мало б статись, якщо Get не повернув помилку
			logError("Об'єкт nil після Get без помилки для %s %s/%s", resTypePrefix, namespace, name)
			displayResourceDetails(finalResType+" ("+resTypePrefix+")", errors.New("API не повернув об'єкт"))
		}
	})
}

func queueUIUpdate(f func()) {
	if fyneApp == nil {
		logError("queueUIUpdate викликано до ініціалізації fyneApp!")
		return
	}
	fyne.Do(f) // Використовуємо правильний метод Do
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
	watcherWg.Add(1)
	// Запускаємо горутину для обробки подій
	go func() {
		defer watcherWg.Done()
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
	// Використовуємо м'ютекс, щоб уникнути гонки при перевірці/встановленні nil
	stateMu.Lock()
	w := watcher
	wd := watcherDone
	stateMu.Unlock()

	if w != nil && wd != nil {
		logInfo("Зупинка file watcher...")
		// Перевіряємо чи канал вже не закритий перед закриттям
		select {
		case <-watcherDone:
			// Вже закрито
		default:
			close(watcherDone) // Сигнал горутині зупинитися
		}
		// <<<--- ДОДАНО: Чекаємо, поки горутина моніторингу завершить свою роботу
		logDebug("Очікування завершення горутини file watcher...")
		watcherWg.Wait()
		logDebug("Горутина file watcher завершилася.")

		// ТІЛЬКИ ПІСЛЯ завершення горутини встановлюємо глобальні змінні в nil
		stateMu.Lock()
		watcher = nil
		watcherDone = nil
		stateMu.Unlock()
		logInfo("File watcher зупинено.")
	} else {
		logDebug("Спроба зупинити file watcher, який не був запущений або вже зупинений.")
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
	fyne.Do(func() {
		rightPanelContainer.Refresh()
	})
	// Оновлюємо статус бар (повідомлення про підключення вже встановлено в connectToCluster)
	// Можна додати інструкцію
	stateMu.RLock()
	ctxListLen := len(allContextNames)
	connCtx := connectedContextName
	stateMu.RUnlock()
	if statusBar != nil {
		fyne.Do(func() {
			statusBar.SetText(fmt.Sprintf("Підключено: %s | Контекстів: %d | Виберіть тип ресурсу", getDisplayName(connCtx), ctxListLen))
		})
	}

}

// --- Головна функція та запуск Fyne ---
func main() {
	log.SetFlags(log.Ldate | log.Ltime)
	logInfo("Запуск " + logPrefix + "...")
	logInfo("Версія Go: %s", runtime.Version())

	initializeLoadingRules()
	setupFileWatcher()

	fyneApp = app.NewWithID(appID)
	mainWindow = fyneApp.NewWindow(appTitle)
	selectedResourceType = "Nodes"

	resIconPng := fyne.NewStaticResource("icon.png", iconData)
	if len(iconData) == 0 {
		logWarning("Дані іконки для трея порожні!")
		resIconPng = nil
	}
	if drv, ok := fyneApp.(desktop.App); ok {
		desktopApp = drv
		if resIconPng != nil {
			desktopApp.SetSystemTrayIcon(resIconPng)
		} else {
			logWarning("Не вдалося встановити іконку трея.")
		}
		trayMenu = buildContextMenu()
		desktopApp.SetSystemTrayMenu(trayMenu)
		logInfo("Системний трей налаштовано.")
	} else {
		desktopApp = nil
		logInfo("Системний трей не підтримується.")
	}

	currentContextLabel = widget.NewLabel(labelLoading)
	statusBar = widget.NewLabel("Ініціалізація...")

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

	resourceTypeTree = widget.NewTree(
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
			return container.NewHBox(widget.NewIcon(theme.FileTextIcon()), widget.NewLabel("Template Resource"))
		},
		func(id widget.TreeNodeID, branch bool, node fyne.CanvasObject) {
			parts := strings.Split(id, "/")
			displayName := parts[len(parts)-1]
			if branch {
				if cont, ok := node.(*fyne.Container); ok && len(cont.Objects) == 2 {
					if lbl, ok2 := cont.Objects[1].(*widget.Label); ok2 {
						lbl.SetText(displayName)
					}
				}
			} else {
				if cont, ok := node.(*fyne.Container); ok && len(cont.Objects) == 2 {
					if lbl, ok2 := cont.Objects[1].(*widget.Label); ok2 {
						lbl.SetText(displayName)
					}
				}
			}
		},
	)
	resourceTypeTree.OnSelected = func(id widget.TreeNodeID) {
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
			resourceTypeTree.Unselect(id)
		}
	}
	resourceTypeTree.OpenBranch("Cluster")
	resourceTypeTree.OpenBranch("Workloads")

	resourceTable = widget.NewTable(
		func() (int, int) {
			stateMu.RLock()
			defer stateMu.RUnlock()
			rows := 0
			cols := 1
			switch selectedResourceType {
			case "Namespaces":
				rows = len(currentNamespaces)
				cols = 3
			case "Nodes":
				rows = len(currentNodes)
				cols = 4
			case "Pods":
				rows = len(currentPods)
				cols = 9
			case "Deployments":
				rows = len(currentDeployments)
				cols = 4
			case "StatefulSets":
				rows = len(currentStatefulSets)
				cols = 4
			case "DaemonSets":
				rows = len(currentDaemonSets)
				cols = 5
			case "ReplicaSets":
				rows = len(currentReplicaSets)
				cols = 5
			case "Jobs":
				rows = len(currentJobs)
				cols = 4
			case "CronJobs":
				rows = len(currentCronJobs)
				cols = 5
			case "ConfigMaps":
				rows = len(currentConfigMaps)
				cols = 3
			case "Secrets":
				rows = len(currentSecrets)
				cols = 4
			case "Services":
				rows = len(currentServices)
				cols = 5
			case "Ingresses":
				rows = len(currentIngresses)
				cols = 4
			case "System Workloads":
				rows = len(currentSystemWorkloads)
				cols = 1
			default:
				rows = 0
				cols = 1
			}
			logDebug("Table Length: Rows=%d, Cols=%d for Type=%s", rows, cols, selectedResourceType)
			return rows, cols
		},
		func() fyne.CanvasObject {
			l := widget.NewLabel("Template")
			l.Truncation = fyne.TextTruncateEllipsis
			return l
		},
		func(id widget.TableCellID, cell fyne.CanvasObject) {
			label := cell.(*widget.Label)
			stateMu.RLock()
			resType := selectedResourceType
			stateMu.RUnlock()
			dataStr := formatCellData(resType, id.Row, id.Col)
			label.SetText(dataStr)
		},
	)
	resourceTable.OnSelected = func(id widget.TableCellID) {
		row := id.Row
		logDebug("Вибрано рядок таблиці: %d", row)
		stateMu.RLock()
		resType := selectedResourceType
		var obj interface{}
		var resourceName, resourceNamespace string
		var fullIdentifier string
		switch resType {
		case "Namespaces":
			if row >= 0 && row < len(currentNamespaces) {
				obj = currentNamespaces[row]
				resourceName = currentNamespaces[row].Name
				resourceNamespace = ""
			}
		case "Nodes":
			if row >= 0 && row < len(currentNodes) {
				obj = currentNodes[row]
				resourceName = currentNodes[row].Name
				resourceNamespace = ""
			}
		case "Pods":
			if row >= 0 && row < len(currentPods) {
				obj = currentPods[row]
				resourceName = currentPods[row].Name
				resourceNamespace = currentPods[row].Namespace
			}
		case "Deployments":
			if row >= 0 && row < len(currentDeployments) {
				obj = currentDeployments[row]
				resourceName = currentDeployments[row].Name
				resourceNamespace = currentDeployments[row].Namespace
			}
		case "StatefulSets":
			if row >= 0 && row < len(currentStatefulSets) {
				obj = currentStatefulSets[row]
				resourceName = currentStatefulSets[row].Name
				resourceNamespace = currentStatefulSets[row].Namespace
			}
		case "DaemonSets":
			if row >= 0 && row < len(currentDaemonSets) {
				obj = currentDaemonSets[row]
				resourceName = currentDaemonSets[row].Name
				resourceNamespace = currentDaemonSets[row].Namespace
			}
		case "ReplicaSets":
			if row >= 0 && row < len(currentReplicaSets) {
				obj = currentReplicaSets[row]
				resourceName = currentReplicaSets[row].Name
				resourceNamespace = currentReplicaSets[row].Namespace
			}
		case "Jobs":
			if row >= 0 && row < len(currentJobs) {
				obj = currentJobs[row]
				resourceName = currentJobs[row].Name
				resourceNamespace = currentJobs[row].Namespace
			}
		case "CronJobs":
			if row >= 0 && row < len(currentCronJobs) {
				obj = currentCronJobs[row]
				resourceName = currentCronJobs[row].Name
				resourceNamespace = currentCronJobs[row].Namespace
			}
		case "ConfigMaps":
			if row >= 0 && row < len(currentConfigMaps) {
				obj = currentConfigMaps[row]
				resourceName = currentConfigMaps[row].Name
				resourceNamespace = currentConfigMaps[row].Namespace
			}
		case "Secrets":
			if row >= 0 && row < len(currentSecrets) {
				obj = currentSecrets[row]
				resourceName = currentSecrets[row].Name
				resourceNamespace = currentSecrets[row].Namespace
			}
		case "Services":
			if row >= 0 && row < len(currentServices) {
				obj = currentServices[row]
				resourceName = currentServices[row].Name
				resourceNamespace = currentServices[row].Namespace
			}
		case "Ingresses":
			if row >= 0 && row < len(currentIngresses) {
				obj = currentIngresses[row]
				resourceName = currentIngresses[row].Name
				resourceNamespace = currentIngresses[row].Namespace
			}
		case "System Workloads":
			if row >= 0 && row < len(currentSystemWorkloads) {
				fullIdentifier = currentSystemWorkloads[row]
				parts := strings.SplitN(fullIdentifier, "/", 2)
				if len(parts) == 2 {
					resourceName = parts[1]
					resourceNamespace = "kube-system"
				} else {
					resourceName = fullIdentifier
					resourceNamespace = "kube-system"
				}
			}
		default:
			logWarning("Вибрано ресурс невідомого типу '%s' для деталей", resType)
		}
		stateMu.RUnlock()
		if resourceName != "" {
			fullName := resourceName
			if resourceNamespace != "" && resType != "Namespaces" && resType != "Nodes" {
				fullName = resourceNamespace + "/" + fullName
			}
			logInfo("Вибрано ресурс '%s': %s", resType, fullName)
			if statusBar != nil {
				statusBar.SetText(fmt.Sprintf("Вибрано %s: %s", resType, fullName))
			}
			if resType == "System Workloads" {
				parts := strings.SplitN(fullIdentifier, "/", 2)
				if len(parts) == 2 {
					go fetchAndDisplayResourceDetails(parts[0], resourceNamespace, resourceName)
				} else {
					logError("Неправильний формат ідентифікатора: %s", fullIdentifier)
					detailWidget := buildErrorDetailsView(resType, fullIdentifier, errors.New("неправильний формат ідентифікатора"))
					if rightPanelContainer != nil {
						rightPanelContainer.Objects = []fyne.CanvasObject{detailWidget}
						rightPanelContainer.Refresh()
					}
				}
			} else if obj != nil {
				displayResourceDetails(resType, obj)
			} else {
				logError("Об'єкт '%s' не знайдено: %s", resType, fullName)
				detailWidget := buildErrorDetailsView(resType, fullName, errors.New("внутрішня помилка: об'єкт не знайдено"))
				if rightPanelContainer != nil && detailWidget != nil {
					rightPanelContainer.Objects = []fyne.CanvasObject{detailWidget}
					rightPanelContainer.Refresh()
				}
			}
		} else {
			logWarning("Не вдалося отримати ідентифікатор для вибраного рядка типу '%s', Row: %d", resType, row)
		}
		resourceTable.Unselect(id)
	}

	leftPanelContent := container.NewVSplit(container.NewBorder(container.NewPadded(widget.NewLabel("Контексти:")), nil, nil, nil, contextListWidget), container.NewBorder(container.NewPadded(widget.NewLabel("Ресурси:")), nil, nil, nil, resourceTypeTree))
	leftPanelContent.Offset = 0.5
	resourceTableHeader = buildTableHeader(selectedResourceType, nil)
	resourceTableContainer := container.NewBorder(resourceTableHeader, nil, nil, nil, resourceTable)
	rightPanelContainer = container.NewMax(resourceTableContainer)
	tappableRightPanel := &tappableContainer{content: rightPanelContainer}
	tappableRightPanel.ExtendBaseWidget(tappableRightPanel)
	split := container.NewHSplit(leftPanelContent, tappableRightPanel)
	split.Offset = 0.3
	mainLayout := container.NewBorder(container.NewVBox(currentContextLabel, widget.NewSeparator()), statusBar, nil, nil, split)
	mainWindow.SetContent(mainLayout)

	mainWindow.Resize(fyne.NewSize(1024, 768))
	mainWindow.CenterOnScreen()
	mainWindow.SetCloseIntercept(func() { logInfo("Закриття вікна..."); stopFileWatcher(); fyneApp.Quit() })

	go loadAndUpdateState()
	mainWindow.ShowAndRun()
	logInfo(logPrefix + " завершено.")
}
