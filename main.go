package main

import (
	"bufio"
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
	"fyne.io/fyne/v2/dialog"
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
	// Cluster
	case "Namespaces":
		return []string{"Name", "Status", "Age"}
	case "Nodes":
		return []string{"Name", "Status", "Roles", "Version", "Age"} // Додано Age
	case "System Workloads":
		return []string{"Component (in kube-system)"} // Як і було

	// Workloads
	case "Pods":
		return []string{"Name", "Namespace", "Ready", "Status", "Restarts", "Controlled By", "Node", "Age"} // Змінено порядок, QoS видалено для стислості
	case "Deployments":
		return []string{"Name", "Namespace", "Ready", "Up-to-date", "Available", "Age"} // Додано колонки статусу
	case "StatefulSets":
		return []string{"Name", "Namespace", "Ready", "Age"} // Як і було
	case "DaemonSets":
		return []string{"Name", "Namespace", "Desired", "Current", "Ready", "Up-to-date", "Available", "Age"} // Додано колонки статусу та Age
	case "ReplicaSets":
		return []string{"Name", "Namespace", "Desired", "Current", "Ready", "Age"} // Додано Age
	case "Jobs":
		return []string{"Name", "Namespace", "Completions", "Duration", "Age"} // Додано Duration
	case "CronJobs":
		return []string{"Name", "Namespace", "Schedule", "Suspend", "Active", "Last Schedule", "Age"} // Додано Active та Age

	// Network
	case "Services":
		return []string{"Name", "Namespace", "Type", "ClusterIP", "ExternalIP", "Ports", "Age"} // Додано ExternalIP та Age
	case "Ingresses":
		return []string{"Name", "Namespace", "Class", "Hosts", "Address", "Ports", "Age"} // Додано Address, Ports, Age

	// Storage
	case "PersistentVolumes":
		return []string{"Name", "Capacity", "Access Modes", "Reclaim Policy", "Status", "Claim", "StorageClass", "Age"} // Нові заголовки
	case "PersistentVolumeClaims":
		return []string{"Name", "Namespace", "Status", "Volume", "Capacity", "Access Modes", "StorageClass", "Age"} // Нові заголовки
	case "StorageClasses":
		return []string{"Name", "Provisioner", "Reclaim Policy", "Volume Binding", "Allow Expansion", "Age"} // Нові заголовки

	// Configuration
	case "ConfigMaps":
		return []string{"Name", "Namespace", "Data Keys", "Age"} // Додано Age
	case "Secrets":
		return []string{"Name", "Namespace", "Type", "Data Keys", "Age"} // Додано Age

	// Access Control
	case "ServiceAccounts":
		return []string{"Name", "Namespace", "Secrets", "Age"} // Нові заголовки
	case "Roles":
		return []string{"Name", "Namespace", "Age"} // Нові заголовки
	case "RoleBindings":
		return []string{"Name", "Namespace", "Role Kind", "Role Name", "Age"} // Нові заголовки
	case "ClusterRoles":
		return []string{"Name", "Age"} // Нові заголовки
	case "ClusterRoleBindings":
		return []string{"Name", "Role Kind", "Role Name", "Age"} // Нові заголовки

	default:
		logWarning("getHeadersForType: Невідомий тип ресурсу '%s', повертається стандартний заголовок", resType)
		return []string{"Name"} // Fallback
	}
}

// Форматує дані для конкретної клітинки таблиці
func formatCellData(resType string, row, col int) string {
	var dataStr = ""
	validRow := false

	stateMu.RLock()
	defer stateMu.RUnlock()

	switch resType {
	// --- Cluster ---
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
			case 4:
				dataStr = formatAge(node.CreationTimestamp) // Додано Age
			}
		}
	case "System Workloads":
		validRow = row >= 0 && row < len(currentSystemWorkloads)
		if validRow {
			if col == 0 {
				dataStr = currentSystemWorkloads[row]
			}
		}

	// --- Workloads ---
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
				dataStr = string(pod.Status.Phase) // Змінено на Status
			case 4:
				dataStr = formatPodRestarts(pod.Status.ContainerStatuses) // Зміщено Restarts
			case 5:
				dataStr = formatOwnerRefs(pod.OwnerReferences) // Зміщено Controlled By
			case 6:
				dataStr = pod.Spec.NodeName // Зміщено Node
			case 7:
				dataStr = formatAge(pod.CreationTimestamp) // Зміщено Age
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
				dataStr = fmt.Sprintf("%d/%d", dep.Status.ReadyReplicas, *dep.Spec.Replicas) // Ready
			case 3:
				dataStr = fmt.Sprintf("%d", dep.Status.UpdatedReplicas) // Up-to-date
			case 4:
				dataStr = fmt.Sprintf("%d", dep.Status.AvailableReplicas) // Available
			case 5:
				dataStr = formatAge(dep.CreationTimestamp) // Age
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
				dataStr = fmt.Sprintf("%d/%d", sts.Status.ReadyReplicas, *sts.Spec.Replicas) // Ready
			case 3:
				dataStr = formatAge(sts.CreationTimestamp) // Age
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
				dataStr = fmt.Sprintf("%d", ds.Status.DesiredNumberScheduled) // Desired
			case 3:
				dataStr = fmt.Sprintf("%d", ds.Status.CurrentNumberScheduled) // Current
			case 4:
				dataStr = fmt.Sprintf("%d", ds.Status.NumberReady) // Ready
			case 5:
				dataStr = fmt.Sprintf("%d", ds.Status.UpdatedNumberScheduled) // Up-to-date
			case 6:
				dataStr = fmt.Sprintf("%d", ds.Status.NumberAvailable) // Available
			case 7:
				dataStr = formatAge(ds.CreationTimestamp) // Age
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
				dataStr = fmt.Sprintf("%d", *rs.Spec.Replicas) // Desired
			case 3:
				dataStr = fmt.Sprintf("%d", rs.Status.Replicas) // Current
			case 4:
				dataStr = fmt.Sprintf("%d", rs.Status.ReadyReplicas) // Ready
			case 5:
				dataStr = formatAge(rs.CreationTimestamp) // Age
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
			case 2: // Completions
				comp := "N/A"
				if job.Spec.Completions != nil {
					comp = fmt.Sprintf("%d/%d", job.Status.Succeeded, *job.Spec.Completions)
				} else {
					comp = fmt.Sprintf("%d/?", job.Status.Succeeded)
				}
				dataStr = comp
			case 3:
				dataStr = formatJobDuration(job.Status) // Duration
			case 4:
				dataStr = formatAge(job.CreationTimestamp) // Age
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
			case 3: // Suspend
				susp := "False"
				if cj.Spec.Suspend != nil && *cj.Spec.Suspend {
					susp = "True"
				}
				dataStr = susp
			case 4:
				dataStr = fmt.Sprintf("%d", len(cj.Status.Active)) // Active
			case 5: // Last Schedule
				last := "Never"
				if cj.Status.LastScheduleTime != nil {
					last = formatAge(*cj.Status.LastScheduleTime)
				}
				dataStr = last
			case 6:
				dataStr = formatAge(cj.CreationTimestamp) // Age
			}
		}

	// --- Network ---
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
				dataStr = strings.Join(svc.Spec.ClusterIPs, ",") // ClusterIP
			case 4:
				dataStr = formatServiceExternalIP(svc) // ExternalIP
			case 5:
				dataStr = formatPorts(svc.Spec.Ports) // Ports
			case 6:
				dataStr = formatAge(svc.CreationTimestamp) // Age
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
			case 2: // Class
				class := "<none>"
				if ing.Spec.IngressClassName != nil {
					class = *ing.Spec.IngressClassName
				}
				dataStr = class
			case 3:
				dataStr = formatIngressHosts(ing.Spec.Rules) // Hosts
			case 4:
				dataStr = formatIngressAddress(ing.Status.LoadBalancer.Ingress) // Address
			case 5:
				dataStr = formatIngressPorts(ing.Spec.TLS) // Ports (from TLS for simplicity)
			case 6:
				dataStr = formatAge(ing.CreationTimestamp) // Age
			}
		}

	// --- Storage ---
	case "PersistentVolumes":
		validRow = row >= 0 && row < len(currentPersistentVolumes)
		if validRow {
			pv := currentPersistentVolumes[row]
			switch col {
			case 0:
				dataStr = pv.Name
			case 1: // Capacity
				storage := pv.Spec.Capacity[corev1.ResourceStorage]
				dataStr = storage.String()
			case 2:
				dataStr = formatAccessModes(pv.Spec.AccessModes) // Access Modes
			case 3:
				dataStr = string(pv.Spec.PersistentVolumeReclaimPolicy) // Reclaim Policy
			case 4:
				dataStr = string(pv.Status.Phase) // Status
			case 5: // Claim
				claim := "<none>"
				if pv.Spec.ClaimRef != nil {
					claim = fmt.Sprintf("%s/%s", pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name)
				}
				dataStr = claim
			case 6: // StorageClass
				sc := pv.Spec.StorageClassName
				if sc == "" {
					sc = "<none>"
				}
				dataStr = sc
			case 7:
				dataStr = formatAge(pv.CreationTimestamp) // Age
			}
		}
	case "PersistentVolumeClaims":
		validRow = row >= 0 && row < len(currentPersistentVolumeClaims)
		if validRow {
			pvc := currentPersistentVolumeClaims[row]
			switch col {
			case 0:
				dataStr = pvc.Name
			case 1:
				dataStr = pvc.Namespace
			case 2:
				dataStr = string(pvc.Status.Phase) // Status
			case 3: // Volume
				vol := pvc.Spec.VolumeName
				if vol == "" {
					vol = "<none>"
				}
				dataStr = vol
			case 4: // Capacity
				storage := pvc.Status.Capacity[corev1.ResourceStorage]
				dataStr = storage.String()
			case 5:
				dataStr = formatAccessModes(pvc.Spec.AccessModes) // Access Modes
			case 6: // StorageClass
				sc := "<none>"
				if pvc.Spec.StorageClassName != nil {
					sc = *pvc.Spec.StorageClassName
				}
				dataStr = sc
			case 7:
				dataStr = formatAge(pvc.CreationTimestamp) // Age
			}
		}
	case "StorageClasses":
		validRow = row >= 0 && row < len(currentStorageClasses)
		if validRow {
			sc := currentStorageClasses[row]
			switch col {
			case 0:
				dataStr = sc.Name
			case 1:
				dataStr = sc.Provisioner
			case 2: // Reclaim Policy
				policy := "<unset>"
				if sc.ReclaimPolicy != nil {
					policy = string(*sc.ReclaimPolicy)
				}
				dataStr = policy
			case 3: // Volume Binding
				mode := "<unset>"
				if sc.VolumeBindingMode != nil {
					mode = string(*sc.VolumeBindingMode)
				}
				dataStr = mode
			case 4: // Allow Expansion
				allow := "<unset>"
				if sc.AllowVolumeExpansion != nil {
					allow = fmt.Sprintf("%t", *sc.AllowVolumeExpansion)
				}
				dataStr = allow
			case 5:
				dataStr = formatAge(sc.CreationTimestamp) // Age
			}
		}

	// --- Configuration ---
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
				dataStr = fmt.Sprintf("%d", len(cm.Data)) // Data Keys
			case 3:
				dataStr = formatAge(cm.CreationTimestamp) // Age
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
				dataStr = string(secret.Type) // Type
			case 3:
				dataStr = fmt.Sprintf("%d", len(secret.Data)) // Data Keys
			case 4:
				dataStr = formatAge(secret.CreationTimestamp) // Age
			}
		}

	// --- Access Control ---
	case "ServiceAccounts":
		validRow = row >= 0 && row < len(currentServiceAccounts)
		if validRow {
			sa := currentServiceAccounts[row]
			switch col {
			case 0:
				dataStr = sa.Name
			case 1:
				dataStr = sa.Namespace
			case 2:
				dataStr = fmt.Sprintf("%d", len(sa.Secrets)) // Secrets count
			case 3:
				dataStr = formatAge(sa.CreationTimestamp) // Age
			}
		}
	case "Roles":
		validRow = row >= 0 && row < len(currentRoles)
		if validRow {
			role := currentRoles[row]
			switch col {
			case 0:
				dataStr = role.Name
			case 1:
				dataStr = role.Namespace
			case 2:
				dataStr = formatAge(role.CreationTimestamp) // Age
			}
		}
	case "RoleBindings":
		validRow = row >= 0 && row < len(currentRoleBindings)
		if validRow {
			rb := currentRoleBindings[row]
			switch col {
			case 0:
				dataStr = rb.Name
			case 1:
				dataStr = rb.Namespace
			case 2:
				dataStr = rb.RoleRef.Kind // Role Kind
			case 3:
				dataStr = rb.RoleRef.Name // Role Name
			case 4:
				dataStr = formatAge(rb.CreationTimestamp) // Age
			}
		}
	case "ClusterRoles":
		validRow = row >= 0 && row < len(currentClusterRoles)
		if validRow {
			cr := currentClusterRoles[row]
			switch col {
			case 0:
				dataStr = cr.Name
			case 1:
				dataStr = formatAge(cr.CreationTimestamp) // Age
			}
		}
	case "ClusterRoleBindings":
		validRow = row >= 0 && row < len(currentClusterRoleBindings)
		if validRow {
			crb := currentClusterRoleBindings[row]
			switch col {
			case 0:
				dataStr = crb.Name
			case 1:
				dataStr = crb.RoleRef.Kind // Role Kind
			case 2:
				dataStr = crb.RoleRef.Name // Role Name
			case 3:
				dataStr = formatAge(crb.CreationTimestamp) // Age
			}
		}

	default:
		// Обробка невідомого типу (можна повернути порожній рядок або повідомлення)
		logWarning("formatCellData: Невідомий тип ресурсу '%s'", resType)
		dataStr = ""
	}

	if !validRow && resType != "" { // Додано перевірку resType != ""
		logDebug("Спроба форматувати недійсну клітинку: Type: %s, Row %d, Col %d", resType, row, col)
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

// Форматує Access Modes для PV/PVC
func formatAccessModes(modes []corev1.PersistentVolumeAccessMode) string {
	s := []string{}
	for _, mode := range modes {
		switch mode {
		case corev1.ReadWriteOnce:
			s = append(s, "RWO")
		case corev1.ReadOnlyMany:
			s = append(s, "ROX")
		case corev1.ReadWriteMany:
			s = append(s, "RWX")
		case corev1.ReadWriteOncePod:
			s = append(s, "RWOP")
		default:
			s = append(s, string(mode))
		}
	}
	if len(s) == 0 {
		return "<none>"
	}
	sort.Strings(s)
	return strings.Join(s, ",")
}

// Форматує тривалість Job
func formatJobDuration(status batchv1.JobStatus) string {
	if status.StartTime == nil {
		return "<pending>"
	}
	if status.CompletionTime == nil {
		// Ще виконується
		return time.Since(status.StartTime.Time).Round(time.Second).String()
	}
	// Завершено
	return status.CompletionTime.Sub(status.StartTime.Time).Round(time.Second).String()
}

// Форматує External IP для Service
func formatServiceExternalIP(svc corev1.Service) string {
	ips := []string{}
	switch svc.Spec.Type {
	case corev1.ServiceTypeLoadBalancer:
		for _, ingress := range svc.Status.LoadBalancer.Ingress {
			if ingress.IP != "" {
				ips = append(ips, ingress.IP)
			}
			if ingress.Hostname != "" {
				ips = append(ips, ingress.Hostname)
			}
		}
	case corev1.ServiceTypeNodePort:
		// Для NodePort покажемо ClusterIP, бо зовнішнього IP немає в стандартному сенсі
		ips = append(ips, svc.Spec.ClusterIPs...)
	case corev1.ServiceTypeExternalName:
		return svc.Spec.ExternalName
	default:
		// Для ClusterIP
		ips = append(ips, svc.Spec.ClusterIPs...)
	}

	if len(ips) == 0 || (len(ips) == 1 && ips[0] == "") {
		return "<none>"
	}
	return strings.Join(ips, ",")
}

// Форматує Address для Ingress
func formatIngressAddress(ingresses []networkingv1.IngressLoadBalancerIngress) string {
	addresses := []string{}
	for _, ingress := range ingresses {
		if ingress.IP != "" {
			addresses = append(addresses, ingress.IP)
		}
		if ingress.Hostname != "" {
			addresses = append(addresses, ingress.Hostname)
		}
	}
	if len(addresses) == 0 {
		return "<none>"
	}
	return strings.Join(addresses, ",")
}

// Форматує порти для Ingress (спрощено, бере з TLS)
func formatIngressPorts(tls []networkingv1.IngressTLS) string {
	ports := []string{}
	has80 := false
	has443 := len(tls) > 0 // Припускаємо, що TLS означає порт 443

	// Можна додати більш складну логіку, аналізуючи правила, але для таблиці це може бути занадто
	// Поки що припускаємо 80 та 443, якщо є TLS
	if !has443 { // Якщо немає TLS, припустимо, що є порт 80
		has80 = true
	} else { // Якщо є TLS, можливо, є і 80? Залежить від конфігурації. Додамо 80 за замовчуванням.
		has80 = true
	}

	if has80 {
		ports = append(ports, "80")
	}
	if has443 {
		ports = append(ports, "443")
	}

	if len(ports) == 0 {
		return "<none>"
	}
	return strings.Join(ports, ",")
}

// --- Оновлення UI віджетів Fyne ---
func updateUIWidgets() {
	logDebug("Оновлення UI віджетів (Fyne)...")
	stateMu.RLock()
	// Отримуємо необхідні дані зі стану (БЕЗ окремих лічильників)
	ctxFromFile := currentContextName
	connCtx := connectedContextName
	ctxList := allContextNames // Зберігаємо список для подальшого використання
	resType := selectedResourceType
	statusTextFromState := ""
	if statusBar != nil {
		statusTextFromState = statusBar.Text
	}
	// ---> ВИДАЛЕНО ОГОЛОШЕННЯ ...Count змінних <---
	stateMu.RUnlock() // Розблоковуємо після читання основного стану

	// --- Оновлення мітки поточного контексту ---
	displayCtxFromFile := labelNoContext
	if ctxFromFile != "" {
		displayCtxFromFile = getDisplayName(ctxFromFile)
	}
	if currentContextLabel != nil {
		queueUIUpdate(func() {
			currentContextLabel.SetText("Поточний у файлі: " + displayCtxFromFile)
		})
	}

	// --- Оновлення списку контекстів та виділення ---
	if contextListWidget != nil {
		targetSelection := connCtx
		if targetSelection == "" {
			targetSelection = ctxFromFile
		}
		selectedIndex := -1
		if targetSelection != "" {
			// ctxList вже прочитано вище під м'ютексом
			for i, name := range ctxList {
				if name == targetSelection {
					selectedIndex = i
					break
				}
			}
		}
		queueUIUpdate(func() {
			contextListWidget.Refresh()
			if selectedIndex != -1 {
				contextListWidget.Select(selectedIndex)
			} else {
				contextListWidget.UnselectAll()
			}
		})
	}

	// --- Оновлення дерева типів ресурсів ---
	if resourceTypeTree != nil {
		queueUIUpdate(func() {
			resourceTypeTree.Refresh()
			if resType != "" {
				resourceTypeTree.Select(resType)
			} else {
				resourceTypeTree.UnselectAll()
			}
		})
	}

	// --- Оновлення таблиці ресурсів та ПІДРАХУНОК КІЛЬКОСТІ ---
	resourceCount := 0 // Ініціалізуємо тут
	if resourceTable != nil {
		// Отримуємо кількість ПРЯМО ТУТ, читаючи довжину зрізів під м'ютексом
		stateMu.RLock() // Блокуємо для читання довжини зрізів
		switch resType {
		case "Namespaces":
			resourceCount = len(currentNamespaces)
		case "Nodes":
			resourceCount = len(currentNodes)
		case "Pods":
			resourceCount = len(currentPods)
		case "Deployments":
			resourceCount = len(currentDeployments)
		case "StatefulSets":
			resourceCount = len(currentStatefulSets)
		case "DaemonSets":
			resourceCount = len(currentDaemonSets)
		case "ReplicaSets":
			resourceCount = len(currentReplicaSets)
		case "Jobs":
			resourceCount = len(currentJobs)
		case "CronJobs":
			resourceCount = len(currentCronJobs)
		case "ConfigMaps":
			resourceCount = len(currentConfigMaps)
		case "Secrets":
			resourceCount = len(currentSecrets)
		case "Services":
			resourceCount = len(currentServices)
		case "Ingresses":
			resourceCount = len(currentIngresses)
		case "ServiceAccounts":
			resourceCount = len(currentServiceAccounts)
		case "Roles":
			resourceCount = len(currentRoles)
		case "RoleBindings":
			resourceCount = len(currentRoleBindings)
		case "ClusterRoles":
			resourceCount = len(currentClusterRoles)
		case "ClusterRoleBindings":
			resourceCount = len(currentClusterRoleBindings)
		case "PersistentVolumes":
			resourceCount = len(currentPersistentVolumes)
		case "PersistentVolumeClaims":
			resourceCount = len(currentPersistentVolumeClaims)
		case "StorageClasses":
			resourceCount = len(currentStorageClasses)
		case "System Workloads":
			resourceCount = len(currentSystemWorkloads)
		default:
			resourceCount = 0
		}
		stateMu.RUnlock() // Розблоковуємо після читання довжини

		logDebug("Оновлення таблиці ресурсів '%s' у UI (%d елементів)", resType, resourceCount)
		queueUIUpdate(func() { resourceTable.Refresh() })
	}

	// --- Оновлення меню трея ---
	updateSystemTrayMenu()

	// --- Оновлення статус-бару ---
	if statusBar != nil {
		canUpdateStatus := !strings.HasPrefix(statusTextFromState, labelError) &&
			!strings.HasPrefix(statusTextFromState, "Підключення") &&
			!strings.HasPrefix(statusTextFromState, labelLoading) &&
			!strings.HasPrefix(statusTextFromState, "Помилка")

		statusString := ""
		// Використовуємо раніше зчитані дані (ctxList вже є)
		if connCtx != "" {
			statusString = fmt.Sprintf("Підключено: %s | ", getDisplayName(connCtx))
		} else if ctxFromFile != "" {
			statusString = fmt.Sprintf("Поточний: %s | ", getDisplayName(ctxFromFile))
		} else {
			statusString = "Не підключено | "
		}
		statusString += fmt.Sprintf("Контекстів: %d", len(ctxList)) // Використовуємо збережену довжину

		if resType != "" {
			// Використовуємо resourceCount, розрахований вище
			statusString += fmt.Sprintf(" | %s: %d", resType, resourceCount)
		} else if connCtx != "" {
			statusString += " | Виберіть тип ресурсу"
		}

		if canUpdateStatus || statusTextFromState == "" || statusTextFromState == labelLoading+"..." {
			queueUIUpdate(func() { statusBar.SetText(statusString) })
		} else {
			logDebug("Збереження поточного статус-бару: %s", statusTextFromState)
		}
	}
	logDebug("Оновлення UI віджетів завершено.")
}

// --- Завантаження даних, оновлення стану та ВИКЛИК оновлення UI ---
func loadAndUpdateState() {
	logInfo("Завантаження конфігурації та оновлення стану...")
	// Безпечне оновлення UI для статусу завантаження
	queueUIUpdate(func() {
		if statusBar != nil {
			statusBar.SetText(labelLoading + "...")
		}
	})

	ctxFromFile, errCtxFile := getCurrentContextFromFile()
	if errCtxFile != nil {
		logError("Не вдалося отримати поточний контекст: %v", errCtxFile)
		// Не встановлюємо помилку в statusBar тут, зробимо це після завантаження списку
		ctxFromFile = "" // Залишаємо порожнім при помилці
	}
	ctxList, errCtxList := getContexts()
	if errCtxList != nil {
		logError("Не вдалося отримати список контекстів: %v", errCtxList)
		// Не встановлюємо помилку в statusBar тут
		ctxList = []string{} // Порожній список при помилці
	}

	// --- Блокування для оновлення стану ---
	stateMu.Lock()
	currentContextName = ctxFromFile
	allContextNames = ctxList
	connectedContextName = "" // Скидаємо підключення
	currentClientset = nil    // Скидаємо клієнт
	selectedResourceType = "" // Скидаємо вибір ресурсу, щоб показати огляд

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
	currentSystemWorkloads = nil
	currentServiceAccounts = nil
	currentRoles = nil
	currentRoleBindings = nil
	currentClusterRoles = nil
	currentClusterRoleBindings = nil
	currentPersistentVolumes = nil
	currentPersistentVolumeClaims = nil
	currentStorageClasses = nil

	stateMu.Unlock()
	// --- Кінець блокування ---

	// Формуємо фінальне повідомлення для statusBar
	var finalStatusMsg string
	if errCtxFile != nil || errCtxList != nil {
		errMsgParts := []string{}
		if errCtxFile != nil {
			errMsgParts = append(errMsgParts, fmt.Sprintf("помилка контексту: %v", errCtxFile))
		}
		if errCtxList != nil {
			errMsgParts = append(errMsgParts, fmt.Sprintf("помилка списку: %v", errCtxList))
		}
		finalStatusMsg = "Помилка завантаження: " + strings.Join(errMsgParts, "; ")
	} else if len(ctxList) == 0 {
		finalStatusMsg = "Немає контекстів у файлі"
	} else {
		finalStatusMsg = fmt.Sprintf("Контекстів: %d. Виберіть контекст.", len(ctxList))
	}

	// Безпечно оновлюємо statusBar та інші віджети
	queueUIUpdate(func() {
		if statusBar != nil {
			statusBar.SetText(finalStatusMsg)
		}
		displayEmptyOverview() // Показуємо порожню панель
		updateUIWidgets()      // Оновлюємо списки, мітки тощо
	})

	logInfo("Завантаження та оновлення стану завершено.")
}

// --- Завантаження вибраних ресурсів ---
func loadSelectedResources() {
	stateMu.RLock()
	clientset := currentClientset
	resType := selectedResourceType // Отримуємо тип з глобальної змінної
	contextName := connectedContextName
	stateMu.RUnlock()

	// Логуємо початок роботи функції
	logDebug("loadSelectedResources викликано з resType = '%s'", resType)

	// Перевірка наявності clientset (крім System Workloads)
	if clientset == nil && resType != "System Workloads" {
		logWarning("Спроба завантажити ресурси Kubernetes без clientset.")
		queueUIUpdate(func() {
			displayEmptyOverview()
			if statusBar != nil {
				statusBar.SetText(fmt.Sprintf("Помилка: Не підключено до '%s' для завантаження %s", getDisplayName(contextName), resType))
			}
			updateUIWidgets()
		})
		return
	}

	logInfo("Завантаження ресурсів типу '%s' для '%s'", resType, contextName)
	// Оновлюємо статус бар, показуючи процес завантаження
	queueUIUpdate(func() {
		if statusBar != nil {
			statusBar.SetText(fmt.Sprintf("Завантаження %s для '%s'...", resType, getDisplayName(contextName)))
		}
		// Можна тимчасово очистити таблицю або показати індикатор
		// displayResourceTableView() // Покажемо порожню таблицю поки вантажаться дані
		// resourceTable.Refresh()
	})

	var loadErr error // Використовуємо одну змінну для збору помилок

	// Локальні змінні для результатів запитів
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
	newServiceAccounts := []corev1.ServiceAccount{}
	newRoles := []rbacv1.Role{}
	newRoleBindings := []rbacv1.RoleBinding{}
	newClusterRoles := []rbacv1.ClusterRole{}
	newClusterRoleBindings := []rbacv1.ClusterRoleBinding{}
	newPVs := []corev1.PersistentVolume{}
	newPVCs := []corev1.PersistentVolumeClaim{}
	newSCs := []storagev1.StorageClass{}

	// Налаштування запиту та контекст з таймаутом
	listOptions := metav1.ListOptions{}
	ctxTimeout, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// --- Очищення старих даних перед завантаженням нових ---
	// Це важливо, щоб не показувати застарілі дані іншого типу
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
	currentServiceAccounts = nil
	currentRoles = nil
	currentRoleBindings = nil
	currentClusterRoles = nil
	currentClusterRoleBindings = nil
	currentPersistentVolumes = nil
	currentPersistentVolumeClaims = nil
	currentStorageClasses = nil
	stateMu.Unlock()
	// --- Кінець очищення ---

	// --- Завантаження даних залежно від типу ресурсу ---
	switch resType {
	// --- Cluster ---
	case "Namespaces":
		list, err := clientset.CoreV1().Namespaces().List(ctxTimeout, listOptions)
		if err != nil {
			loadErr = errors.Join(loadErr, fmt.Errorf("namespaces: %w", err))
		} else {
			newNamespaces = list.Items
			sort.Slice(newNamespaces, func(i, j int) bool { return newNamespaces[i].Name < newNamespaces[j].Name })
		}
	case "Nodes":
		list, err := clientset.CoreV1().Nodes().List(ctxTimeout, listOptions)
		if err != nil {
			loadErr = errors.Join(loadErr, fmt.Errorf("nodes: %w", err))
		} else {
			newNodes = list.Items
			sort.Slice(newNodes, func(i, j int) bool { return newNodes[i].Name < newNodes[j].Name })
		}

	// --- Workloads ---
	case "Pods":
		list, err := clientset.CoreV1().Pods("").List(ctxTimeout, listOptions) // Усі неймспейси
		if err != nil {
			loadErr = errors.Join(loadErr, fmt.Errorf("pods: %w", err))
		} else {
			newPods = list.Items
			sort.Slice(newPods, func(i, j int) bool {
				if newPods[i].Namespace != newPods[j].Namespace {
					return newPods[i].Namespace < newPods[j].Namespace
				}
				return newPods[i].Name < newPods[j].Name
			})
		}
	case "Deployments":
		list, err := clientset.AppsV1().Deployments("").List(ctxTimeout, listOptions) // Усі неймспейси
		if err != nil {
			loadErr = errors.Join(loadErr, fmt.Errorf("deployments: %w", err))
		} else {
			newDeployments = list.Items
			sort.Slice(newDeployments, func(i, j int) bool {
				if newDeployments[i].Namespace != newDeployments[j].Namespace {
					return newDeployments[i].Namespace < newDeployments[j].Namespace
				}
				return newDeployments[i].Name < newDeployments[j].Name
			})
		}
	case "StatefulSets": // <-- Додано
		list, err := clientset.AppsV1().StatefulSets("").List(ctxTimeout, listOptions) // Усі неймспейси
		if err != nil {
			loadErr = errors.Join(loadErr, fmt.Errorf("statefulsets: %w", err))
		} else {
			newStatefulSets = list.Items
			sort.Slice(newStatefulSets, func(i, j int) bool {
				if newStatefulSets[i].Namespace != newStatefulSets[j].Namespace {
					return newStatefulSets[i].Namespace < newStatefulSets[j].Namespace
				}
				return newStatefulSets[i].Name < newStatefulSets[j].Name
			})
		}
	case "DaemonSets": // <-- Додано
		list, err := clientset.AppsV1().DaemonSets("").List(ctxTimeout, listOptions) // Усі неймспейси
		if err != nil {
			loadErr = errors.Join(loadErr, fmt.Errorf("daemonsets: %w", err))
		} else {
			newDaemonSets = list.Items
			sort.Slice(newDaemonSets, func(i, j int) bool {
				if newDaemonSets[i].Namespace != newDaemonSets[j].Namespace {
					return newDaemonSets[i].Namespace < newDaemonSets[j].Namespace
				}
				return newDaemonSets[i].Name < newDaemonSets[j].Name
			})
		}
	case "ReplicaSets": // <-- Додано
		list, err := clientset.AppsV1().ReplicaSets("").List(ctxTimeout, listOptions) // Усі неймспейси
		if err != nil {
			loadErr = errors.Join(loadErr, fmt.Errorf("replicasets: %w", err))
		} else {
			newReplicaSets = list.Items
			sort.Slice(newReplicaSets, func(i, j int) bool {
				if newReplicaSets[i].Namespace != newReplicaSets[j].Namespace {
					return newReplicaSets[i].Namespace < newReplicaSets[j].Namespace
				}
				return newReplicaSets[i].Name < newReplicaSets[j].Name
			})
		}
	case "Jobs": // <-- Додано
		list, err := clientset.BatchV1().Jobs("").List(ctxTimeout, listOptions) // Усі неймспейси
		if err != nil {
			loadErr = errors.Join(loadErr, fmt.Errorf("jobs: %w", err))
		} else {
			newJobs = list.Items
			sort.Slice(newJobs, func(i, j int) bool {
				if newJobs[i].Namespace != newJobs[j].Namespace {
					return newJobs[i].Namespace < newJobs[j].Namespace
				}
				return newJobs[i].Name < newJobs[j].Name
			})
		}
	case "CronJobs": // <-- Додано
		list, err := clientset.BatchV1().CronJobs("").List(ctxTimeout, listOptions) // Усі неймспейси
		if err != nil {
			loadErr = errors.Join(loadErr, fmt.Errorf("cronjobs: %w", err))
		} else {
			newCronJobs = list.Items
			sort.Slice(newCronJobs, func(i, j int) bool {
				if newCronJobs[i].Namespace != newCronJobs[j].Namespace {
					return newCronJobs[i].Namespace < newCronJobs[j].Namespace
				}
				return newCronJobs[i].Name < newCronJobs[j].Name
			})
		}

	// --- Network ---
	case "Services": // <-- Додано
		list, err := clientset.CoreV1().Services("").List(ctxTimeout, listOptions) // Усі неймспейси
		if err != nil {
			loadErr = errors.Join(loadErr, fmt.Errorf("services: %w", err))
		} else {
			newServices = list.Items
			sort.Slice(newServices, func(i, j int) bool {
				if newServices[i].Namespace != newServices[j].Namespace {
					return newServices[i].Namespace < newServices[j].Namespace
				}
				return newServices[i].Name < newServices[j].Name
			})
		}
	case "Ingresses": // <-- Додано
		list, err := clientset.NetworkingV1().Ingresses("").List(ctxTimeout, listOptions) // Усі неймспейси
		if err != nil {
			loadErr = errors.Join(loadErr, fmt.Errorf("ingresses: %w", err))
		} else {
			newIngresses = list.Items
			sort.Slice(newIngresses, func(i, j int) bool {
				if newIngresses[i].Namespace != newIngresses[j].Namespace {
					return newIngresses[i].Namespace < newIngresses[j].Namespace
				}
				return newIngresses[i].Name < newIngresses[j].Name
			})
		}

	// --- Configuration ---
	case "ConfigMaps": // <-- Додано
		list, err := clientset.CoreV1().ConfigMaps("").List(ctxTimeout, listOptions) // Усі неймспейси
		if err != nil {
			loadErr = errors.Join(loadErr, fmt.Errorf("configmaps: %w", err))
		} else {
			newConfigMaps = list.Items
			sort.Slice(newConfigMaps, func(i, j int) bool {
				if newConfigMaps[i].Namespace != newConfigMaps[j].Namespace {
					return newConfigMaps[i].Namespace < newConfigMaps[j].Namespace
				}
				return newConfigMaps[i].Name < newConfigMaps[j].Name
			})
		}
	case "Secrets": // <-- Додано
		list, err := clientset.CoreV1().Secrets("").List(ctxTimeout, listOptions) // Усі неймспейси
		if err != nil {
			loadErr = errors.Join(loadErr, fmt.Errorf("secrets: %w", err))
		} else {
			newSecrets = list.Items
			sort.Slice(newSecrets, func(i, j int) bool {
				if newSecrets[i].Namespace != newSecrets[j].Namespace {
					return newSecrets[i].Namespace < newSecrets[j].Namespace
				}
				return newSecrets[i].Name < newSecrets[j].Name
			})
		}

	// --- Storage ---
	case "PersistentVolumes": // <-- Додано
		list, err := clientset.CoreV1().PersistentVolumes().List(ctxTimeout, listOptions) // Кластерний ресурс
		if err != nil {
			loadErr = errors.Join(loadErr, fmt.Errorf("persistentvolumes: %w", err))
		} else {
			newPVs = list.Items
			sort.Slice(newPVs, func(i, j int) bool { return newPVs[i].Name < newPVs[j].Name })
		}
	case "PersistentVolumeClaims": // <-- Додано
		list, err := clientset.CoreV1().PersistentVolumeClaims("").List(ctxTimeout, listOptions) // Усі неймспейси
		if err != nil {
			loadErr = errors.Join(loadErr, fmt.Errorf("persistentvolumeclaims: %w", err))
		} else {
			newPVCs = list.Items
			sort.Slice(newPVCs, func(i, j int) bool {
				if newPVCs[i].Namespace != newPVCs[j].Namespace {
					return newPVCs[i].Namespace < newPVCs[j].Namespace
				}
				return newPVCs[i].Name < newPVCs[j].Name
			})
		}
	case "StorageClasses": // <-- Додано
		list, err := clientset.StorageV1().StorageClasses().List(ctxTimeout, listOptions) // Кластерний ресурс
		if err != nil {
			loadErr = errors.Join(loadErr, fmt.Errorf("storageclasses: %w", err))
		} else {
			newSCs = list.Items
			sort.Slice(newSCs, func(i, j int) bool { return newSCs[i].Name < newSCs[j].Name })
		}

	// --- Access Control ---
	case "ServiceAccounts": // <-- Додано
		list, err := clientset.CoreV1().ServiceAccounts("").List(ctxTimeout, listOptions) // Усі неймспейси
		if err != nil {
			loadErr = errors.Join(loadErr, fmt.Errorf("serviceaccounts: %w", err))
		} else {
			newServiceAccounts = list.Items
			sort.Slice(newServiceAccounts, func(i, j int) bool {
				if newServiceAccounts[i].Namespace != newServiceAccounts[j].Namespace {
					return newServiceAccounts[i].Namespace < newServiceAccounts[j].Namespace
				}
				return newServiceAccounts[i].Name < newServiceAccounts[j].Name
			})
		}
	case "Roles": // <-- Додано
		list, err := clientset.RbacV1().Roles("").List(ctxTimeout, listOptions) // Усі неймспейси
		if err != nil {
			loadErr = errors.Join(loadErr, fmt.Errorf("roles: %w", err))
		} else {
			newRoles = list.Items
			sort.Slice(newRoles, func(i, j int) bool {
				if newRoles[i].Namespace != newRoles[j].Namespace {
					return newRoles[i].Namespace < newRoles[j].Namespace
				}
				return newRoles[i].Name < newRoles[j].Name
			})
		}
	case "RoleBindings": // <-- Додано
		list, err := clientset.RbacV1().RoleBindings("").List(ctxTimeout, listOptions) // Усі неймспейси
		if err != nil {
			loadErr = errors.Join(loadErr, fmt.Errorf("rolebindings: %w", err))
		} else {
			newRoleBindings = list.Items
			sort.Slice(newRoleBindings, func(i, j int) bool {
				if newRoleBindings[i].Namespace != newRoleBindings[j].Namespace {
					return newRoleBindings[i].Namespace < newRoleBindings[j].Namespace
				}
				return newRoleBindings[i].Name < newRoleBindings[j].Name
			})
		}
	case "ClusterRoles": // <-- Додано
		list, err := clientset.RbacV1().ClusterRoles().List(ctxTimeout, listOptions) // Кластерний ресурс
		if err != nil {
			loadErr = errors.Join(loadErr, fmt.Errorf("clusterroles: %w", err))
		} else {
			newClusterRoles = list.Items
			sort.Slice(newClusterRoles, func(i, j int) bool { return newClusterRoles[i].Name < newClusterRoles[j].Name })
		}
	case "ClusterRoleBindings": // <-- Додано
		list, err := clientset.RbacV1().ClusterRoleBindings().List(ctxTimeout, listOptions) // Кластерний ресурс
		if err != nil {
			loadErr = errors.Join(loadErr, fmt.Errorf("clusterrolebindings: %w", err))
		} else {
			newClusterRoleBindings = list.Items
			sort.Slice(newClusterRoleBindings, func(i, j int) bool { return newClusterRoleBindings[i].Name < newClusterRoleBindings[j].Name })
		}

	// --- System Workloads (Збір з кількох типів) ---
	case "System Workloads":
		logInfo("Завантаження системних компонентів з kube-system...")
		workloadNamespace := "kube-system"
		var sysErr error // Окрема змінна для помилок цієї секції

		depList, depErr := clientset.AppsV1().Deployments(workloadNamespace).List(ctxTimeout, listOptions)
		if depErr != nil {
			sysErr = errors.Join(sysErr, fmt.Errorf("sys-deploy: %w", depErr))
		} else {
			for _, item := range depList.Items {
				newSystemWorkloads = append(newSystemWorkloads, fmt.Sprintf("deploy/%s", item.Name))
			}
		}
		dsList, dsErr := clientset.AppsV1().DaemonSets(workloadNamespace).List(ctxTimeout, listOptions)
		if dsErr != nil {
			sysErr = errors.Join(sysErr, fmt.Errorf("sys-ds: %w", dsErr))
		} else {
			for _, item := range dsList.Items {
				newSystemWorkloads = append(newSystemWorkloads, fmt.Sprintf("ds/%s", item.Name))
			}
		}
		stsList, stsErr := clientset.AppsV1().StatefulSets(workloadNamespace).List(ctxTimeout, listOptions)
		if stsErr != nil {
			sysErr = errors.Join(sysErr, fmt.Errorf("sys-sts: %w", stsErr))
		} else {
			for _, item := range stsList.Items {
				newSystemWorkloads = append(newSystemWorkloads, fmt.Sprintf("sts/%s", item.Name))
			}
		}
		sort.Strings(newSystemWorkloads)
		loadErr = errors.Join(loadErr, sysErr) // Додаємо помилки системних ворклоадів до загальної

	default:
		logWarning("Невідомий тип ресурсу для завантаження у switch: %s", resType)
		loadErr = errors.Join(loadErr, fmt.Errorf("тип %s не підтримується", resType))
	}
	// --- Кінець завантаження даних ---

	// --- Оновлення глобального стану ---
	stateMu.Lock()
	// Присвоюємо ЗАВАНТАЖЕНІ дані відповідним глобальним змінним
	// Важливо: робимо це навіть якщо були помилки, щоб показати те, що вдалося завантажити
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
		currentStatefulSets = newStatefulSets // <-- Додано
	case "DaemonSets":
		currentDaemonSets = newDaemonSets // <-- Додано
	case "ReplicaSets":
		currentReplicaSets = newReplicaSets // <-- Додано
	case "Jobs":
		currentJobs = newJobs // <-- Додано
	case "CronJobs":
		currentCronJobs = newCronJobs // <-- Додано
	case "ConfigMaps":
		currentConfigMaps = newConfigMaps // <-- Додано
	case "Secrets":
		currentSecrets = newSecrets // <-- Додано
	case "Services":
		currentServices = newServices // <-- Додано
	case "Ingresses":
		currentIngresses = newIngresses // <-- Додано
	case "PersistentVolumes":
		currentPersistentVolumes = newPVs // <-- Додано
	case "PersistentVolumeClaims":
		currentPersistentVolumeClaims = newPVCs // <-- Додано
	case "StorageClasses":
		currentStorageClasses = newSCs // <-- Додано
	case "ServiceAccounts":
		currentServiceAccounts = newServiceAccounts // <-- Додано
	case "Roles":
		currentRoles = newRoles // <-- Додано
	case "RoleBindings":
		currentRoleBindings = newRoleBindings // <-- Додано
	case "ClusterRoles":
		currentClusterRoles = newClusterRoles // <-- Додано
	case "ClusterRoleBindings":
		currentClusterRoleBindings = newClusterRoleBindings // <-- Додано
	case "System Workloads":
		currentSystemWorkloads = newSystemWorkloads
	}
	// Отримуємо кількість завантажених елементів для статусу
	// Цей switch також має бути повним
	resourceCount := 0
	switch resType {
	case "Namespaces":
		resourceCount = len(currentNamespaces)
	case "Nodes":
		resourceCount = len(currentNodes)
	case "Pods":
		resourceCount = len(currentPods)
	case "Deployments":
		resourceCount = len(currentDeployments)
	case "StatefulSets":
		resourceCount = len(currentStatefulSets) // <-- Додано
	case "DaemonSets":
		resourceCount = len(currentDaemonSets) // <-- Додано
	case "ReplicaSets":
		resourceCount = len(currentReplicaSets) // <-- Додано
	case "Jobs":
		resourceCount = len(currentJobs) // <-- Додано
	case "CronJobs":
		resourceCount = len(currentCronJobs) // <-- Додано
	case "ConfigMaps":
		resourceCount = len(currentConfigMaps) // <-- Додано
	case "Secrets":
		resourceCount = len(currentSecrets) // <-- Додано
	case "Services":
		resourceCount = len(currentServices) // <-- Додано
	case "Ingresses":
		resourceCount = len(currentIngresses) // <-- Додано
	case "PersistentVolumes":
		resourceCount = len(currentPersistentVolumes) // <-- Додано
	case "PersistentVolumeClaims":
		resourceCount = len(currentPersistentVolumeClaims) // <-- Додано
	case "StorageClasses":
		resourceCount = len(currentStorageClasses) // <-- Додано
	case "ServiceAccounts":
		resourceCount = len(currentServiceAccounts) // <-- Додано
	case "Roles":
		resourceCount = len(currentRoles) // <-- Додано
	case "RoleBindings":
		resourceCount = len(currentRoleBindings) // <-- Додано
	case "ClusterRoles":
		resourceCount = len(currentClusterRoles) // <-- Додано
	case "ClusterRoleBindings":
		resourceCount = len(currentClusterRoleBindings) // <-- Додано
	case "System Workloads":
		resourceCount = len(currentSystemWorkloads)
	default:
		resourceCount = 0
	}
	stateMu.Unlock()
	// --- Кінець оновлення стану ---

	// --- Оновлення UI ---
	queueUIUpdate(func() {
		finalStatusMsg := ""
		if loadErr != nil {
			logError("Помилка(и) завантаження %s для '%s': %v", resType, contextName, loadErr)
			finalStatusMsg = fmt.Sprintf("Помилка %s: %v", resType, loadErr) // Показуємо зібрані помилки
			displayResourceTableView()                                       // Показуємо таблицю (можливо, частково заповнену)
		} else {
			finalStatusMsg = fmt.Sprintf("Підключено: %s | %s: %d", getDisplayName(contextName), resType, resourceCount)
			displayResourceTableView() // Показуємо таблицю з даними
		}

		if statusBar != nil {
			statusBar.SetText(finalStatusMsg)
		}
		// Цей виклик оновить таблицю (викличе її Length(), CreateHeader()...),
		// а також інші віджети (дерево, список контекстів)
		updateUIWidgets()
	})

	logInfo("Завантаження '%s' завершено (з можливими помилками: %v).", resType, loadErr) // Додамо помилку в лог завершення
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

// Показує таблицю ресурсів (resourceTable) у правій панелі
func displayResourceTableView() {
	logDebug("Показ таблиці ресурсів (resourceTable)")
	if rightPanelContainer != nil && resourceTable != nil {
		// Переконуємося, що саме resourceTable видимий
		if len(rightPanelContainer.Objects) == 0 || rightPanelContainer.Objects[0] != resourceTable {
			rightPanelContainer.Objects = []fyne.CanvasObject{resourceTable}
		}
		// Потрібно оновити і саму таблицю, і контейнер Max
		// Оновлення самої таблиці відбувається через updateUIWidgets -> resourceTable.Refresh()
		rightPanelContainer.Refresh() // Оновлюємо контейнер
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
	detailsVBox := container.NewVBox() // Основний контейнер для деталей

	// --- Кнопки дій ---
	actionsBox := container.NewHBox()
	logsButton := widget.NewButtonWithIcon("View Logs", theme.ListIcon(), func() {
		// Обробник натискання кнопки логів
		logInfo("Запит логів для Pod: %s/%s", pod.Namespace, pod.Name)

		numContainers := len(pod.Spec.Containers)
		if numContainers == 0 {
			logWarning("У пода %s/%s немає контейнерів.", pod.Namespace, pod.Name)
			// Використовуємо queueUIUpdate для показу діалогу з горутини/обробника
			queueUIUpdate(func() {
				dialog.ShowInformation("Інформація", "У вибраного пода немає контейнерів.", mainWindow)
			})
			return
		}

		// Якщо один контейнер, відкриваємо вікно логів одразу
		if numContainers == 1 {
			containerName := pod.Spec.Containers[0].Name
			logDebug("Один контейнер знайдено: %s. Відкриття вікна логів.", containerName)
			// Не потрібно queueUIUpdate, бо showLogWindow сама керує UI
			showLogWindow(pod.Namespace, pod.Name, containerName)
			return
		}

		// --- Використання ShowCustomConfirm для вибору контейнера ---
		logDebug("Знайдено %d контейнерів. Створення діалогу вибору через ShowCustomConfirm.", numContainers)
		var containerNames []string
		for _, c := range pod.Spec.Containers {
			containerNames = append(containerNames, c.Name)
		}

		// Змінна для зберігання вибраного контейнера
		var selectedContainer string
		// Встановлюємо перший контейнер як вибраний за замовчуванням
		if len(containerNames) > 0 {
			selectedContainer = containerNames[0]
		}

		// Створюємо RadioGroup для вибору
		radioGroup := widget.NewRadioGroup(containerNames, func(selected string) {
			logDebug("RadioGroup OnChanged: %s", selected)
			selectedContainer = selected // Оновлюємо змінну при зміні вибору
		})
		radioGroup.SetSelected(selectedContainer) // Встановлюємо вибір за замовчуванням

		// Створюємо та показуємо кастомний діалог (використовуємо queueUIUpdate для показу діалогу з обробника)
		queueUIUpdate(func() {
			confirmDialog := dialog.NewCustomConfirm(
				"Виберіть контейнер",             // Title
				"Вибрати",                        // Confirm button text
				"Скасувати",                      // Dismiss button text
				container.NewVScroll(radioGroup), // Content (RadioGroup у скролі)
				func(confirm bool) { // Callback function
					if confirm {
						// Користувач натиснув "Вибрати"
						if selectedContainer != "" {
							logDebug("Підтверджено вибір контейнера: %s. Відкриття вікна логів.", selectedContainer)
							// Не потрібно queueUIUpdate, бо showLogWindow сама керує UI
							showLogWindow(pod.Namespace, pod.Name, selectedContainer)
						} else {
							logWarning("Підтверджено вибір, але selectedContainer порожній.")
						}
					} else {
						// Користувач натиснув "Скасувати"
						logDebug("Вибір контейнера скасовано.")
					}
				},
				mainWindow, // Parent window
			)
			// Встановлюємо мінімальний розмір діалогу (опціонально)
			confirmDialog.Resize(fyne.NewSize(300, 200))
			confirmDialog.Show()
		})
		// --- Кінець використання ShowCustomConfirm ---
	})
	actionsBox.Add(logsButton) // Додаємо кнопку до HBox
	// Можна додати інші кнопки дій тут (наприклад, Exec, Delete)

	// --- Додаємо інформацію про под ---
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
	// --- Кінець інформації про под ---

	// Кнопка "Назад"
	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })

	// Компонуємо вигляд: Кнопки дій зверху, кнопка Назад знизу, деталі в центрі
	return container.NewBorder(
		container.NewVBox(actionsBox, widget.NewSeparator()), // Top: Actions box and separator
		backButton,                        // Bottom: Back button
		nil,                               // Left
		nil,                               // Right
		container.NewVScroll(detailsVBox), // Center: Scrollable details
	)
}

// Створює та показує нове вікно для відображення логів контейнера
func showLogWindow(namespace, podName, containerName string) {
	logWindow := fyneApp.NewWindow(fmt.Sprintf("Logs - %s/%s (%s)", namespace, podName, containerName))
	logWindow.Resize(fyne.NewSize(800, 600))

	// Створюємо багаторядкове текстове поле для логів (тільки для читання)
	logEntry := widget.NewMultiLineEntry()
	logEntry.Disable()                   // Робимо його нередагованим
	logEntry.Wrapping = fyne.TextWrapOff // Вимикаємо перенос рядків для логів

	// Створюємо скрол для текстового поля
	logScroll := container.NewScroll(logEntry)

	// Чекбокс для ввімкнення/вимкнення стрімінгу (слідування за логами)
	followCheck := widget.NewCheck("Follow", nil)
	followCheck.SetChecked(true) // За замовчуванням слідуємо

	// Контекст для керування горутиною стрімінгу
	// Він буде скасований при закритті вікна
	ctx, cancelFunc := context.WithCancel(context.Background())

	// Запускаємо горутину для отримання та відображення логів
	// Передаємо контекст, дані пода, віджети та функцію скасування
	go streamLogs(ctx, namespace, podName, containerName, logEntry, followCheck, logScroll)

	// Встановлюємо обробник закриття вікна, який скасує контекст
	logWindow.SetOnClosed(func() {
		logInfo("Вікно логів для %s/%s (%s) закривається. Зупинка стрімінгу.", namespace, podName, containerName)
		cancelFunc() // Скасовуємо контекст, що зупинить горутину streamLogs
	})

	// --- Компонування вікна логів ---
	topToolbar := container.NewHBox(
		widget.NewLabel(fmt.Sprintf("%s/%s [%s]", namespace, podName, containerName)),
		// Можна додати інші елементи керування тут (наприклад, вибір кількості рядків)
		followCheck,
	)

	logWindowContent := container.NewBorder(
		topToolbar, // Top: інформація та чекбокс
		nil,        // Bottom
		nil,        // Left
		nil,        // Right
		logScroll,  // Center: скрольоване поле з логами
	)

	logWindow.SetContent(logWindowContent)
	logWindow.Show()
}

// Отримує та стрімить логи для вказаного контейнера пода з батчингом (спрощена версія без окремої горутини відправки)
func streamLogs(ctx context.Context, namespace, podName, containerName string,
	entry *widget.Entry, followCheck *widget.Check, scroll *container.Scroll) {

	logDebug("streamLogs: Starting for %s/%s [%s]", namespace, podName, containerName)
	// Лог при виході з функції для діагностики
	defer logDebug("streamLogs: Exiting for %s/%s [%s]", namespace, podName, containerName)

	stateMu.RLock()
	clientset := currentClientset
	stateMu.RUnlock()

	if clientset == nil {
		logError("streamLogs: Немає активного clientset.")
		queueUIUpdate(func() { entry.SetText("Помилка: Немає підключення до кластера.") })
		return
	}

	// --- Змінні для батчингу ---
	var logBuffer strings.Builder
	var bufferMutex sync.Mutex // М'ютекс все ще потрібен, якщо доступ до буфера можливий з UI (хоча тут наче ні)
	bufferSize := 0
	maxBufferSize := 50 // Батч по 50 рядків

	// --- Функція для відправки буфера в UI ---
	// Викликається напряму з основного потоку горутини streamLogs
	flushBuffer := func() {
		bufferMutex.Lock()
		if bufferSize == 0 {
			bufferMutex.Unlock()
			return
		}
		logsToSend := logBuffer.String()
		logBuffer.Reset()
		//currentSize := bufferSize
		bufferSize = 0
		bufferMutex.Unlock()

		queueUIUpdate(func() { // Використовуємо fyne.Do
			// logDebug("Flushing %d log lines to UI", currentSize) // Можна закоментувати для чистоти логів
			prefix := ""
			if entry.Text != "" && !strings.HasSuffix(entry.Text, "\n") && len(logsToSend) > 0 {
				prefix = "\n"
			}
			entry.Append(prefix + logsToSend)
			if followCheck.Checked && scroll != nil {
				scroll.ScrollToBottom()
			}
		})
	}
	// --- Кінець flushBuffer ---

	// --- Функція для додавання рядка в буфер і перевірки на заповнення ---
	addLineToBuffer := func(line string) {
		bufferMutex.Lock()
		logBuffer.WriteString(line + "\n")
		bufferSize++
		shouldFlushNow := bufferSize >= maxBufferSize
		bufferMutex.Unlock()

		if shouldFlushNow {
			// logDebug("Log buffer full (%d lines), flushing.", maxBufferSize)
			flushBuffer() // Відправляємо одразу
		}
	}
	// --- Кінець addLineToBuffer ---

	// Гарантована відправка залишків буфера при виході з функції streamLogs
	// Важливо: defer виконується останнім, після всіх return
	defer func() {
		logDebug("streamLogs: Flushing remaining buffer on exit for %s/%s [%s]", namespace, podName, containerName)
		flushBuffer()
	}()

	// --- Початкове завантаження ---
	var tailLines int64 = 100
	opts := &corev1.PodLogOptions{Container: containerName, TailLines: &tailLines, Timestamps: true}
	logDebug("Завантаження початкових %d рядків...", tailLines)
	req := clientset.CoreV1().Pods(namespace).GetLogs(podName, opts)
	stream, err := req.Stream(ctx) // Використовуємо контекст вікна
	if err != nil {
		logError("Помилка отримання початкового stream: %v", err)
		queueUIUpdate(func() { entry.SetText(fmt.Sprintf("Помилка отримання логів:\n%v", err)) })
		return // defer flushBuffer() спрацює
	}

	scanner := bufio.NewScanner(stream)
	initialLineCount := 0
	for scanner.Scan() {
		// Перевіряємо контекст ПЕРЕД обробкою рядка
		select {
		case <-ctx.Done():
			logInfo("Скасовано під час читання початкових логів.")
			stream.Close() // Закриваємо потік
			return         // defer flushBuffer() спрацює
		default:
			addLineToBuffer(scanner.Text())
			initialLineCount++
		}
	}
	stream.Close() // Закриваємо потік після читання
	logDebug("Прочитано %d початкових рядків.", initialLineCount)
	if errScan := scanner.Err(); errScan != nil {
		// Перевіряємо чи помилка не через скасування контексту
		if !errors.Is(errScan, context.Canceled) && ctx.Err() != context.Canceled {
			logError("Помилка сканера (початкові логи): %v", errScan)
			addLineToBuffer(fmt.Sprintf("\nПОМИЛКА ЧИТАННЯ ПОЧАТКОВИХ ЛОГІВ: %v\n", errScan))
		} else {
			logInfo("Сканер початкових логів завершився через скасування контексту.")
			return // defer flushBuffer() спрацює
		}
	}
	// Не потрібно примусової відправки тут, defer відправить або наступний етап

	// --- Стрімінг (Follow=true) ---
	for { // Зовнішній цикл для перепідключення
		// Перевірка контексту на початку кожної спроби перепідключення
		select {
		case <-ctx.Done():
			logInfo("Стрімінг зупинено перед циклом перепідключення (контекст скасовано).")
			return
		default:
		}

		// Перевіряємо Follow чекбокс
		if !followCheck.Checked {
			logDebug("Follow вимкнено, завершення streamLogs.")
			return // Якщо слідування вимкнене, завершуємо горутину
		}

		logDebug("Запуск/перезапуск стрімінгу (Follow=true)...")
		streamOpts := &corev1.PodLogOptions{Container: containerName, Follow: true, Timestamps: true, SinceTime: &metav1.Time{Time: time.Now()}} // Додано SinceTime
		reqFollow := clientset.CoreV1().Pods(namespace).GetLogs(podName, streamOpts)
		streamFollow, errFollow := reqFollow.Stream(ctx) // Використовуємо контекст вікна

		if errFollow != nil {
			// Якщо контекст скасовано, це очікувана помилка при закритті вікна
			if errors.Is(errFollow, context.Canceled) || ctx.Err() == context.Canceled {
				logInfo("Не вдалося почати стрімінг, оскільки контекст скасовано.")
				return // Виходимо
			}
			logError("Помилка отримання Follow stream: %v", errFollow)
			addLineToBuffer(fmt.Sprintf("\nПОМИЛКА СТРІМІНГУ: %v\n", errFollow))
			flushBuffer() // Відправляємо помилку одразу
			// Пауза перед повторною спробою
			select {
			case <-time.After(5 * time.Second):
				continue // Наступна ітерація циклу for
			case <-ctx.Done():
				logInfo("Стрімінг зупинено після помилки підключення під час паузи (контекст скасовано).")
				return
			}
		}

		// Читання потоку
		scannerFollow := bufio.NewScanner(streamFollow)
		streamLineCount := 0
	ScanLoopFollow:
		for scannerFollow.Scan() {
			// Перевіряємо контекст ПЕРЕД обробкою рядка
			select {
			case <-ctx.Done():
				logInfo("Стрімінг перервано під час читання (контекст скасовано).")
				streamFollow.Close()
				break ScanLoopFollow // Виходимо з внутрішнього циклу сканера
			default:
				// Follow перевіряємо ТУТ, щоб не пропустити вимкнення
				if !followCheck.Checked {
					logDebug("Follow вимкнено під час читання потоку.")
					streamFollow.Close()
					break ScanLoopFollow
				}
				addLineToBuffer(scannerFollow.Text())
				streamLineCount++
			}
		} // Кінець for scannerFollow.Scan()

		streamFollow.Close() // Закриваємо потік після виходу з ScanLoopFollow
		logDebug("Прочитано %d рядків у поточному сеансі стрімінгу.", streamLineCount)
		// flushBuffer() // Не викликаємо тут, defer зробить це гарантовано при виході

		// Перевірка контексту ПІСЛЯ виходу з внутрішнього циклу
		select {
		case <-ctx.Done():
			logInfo("Стрімінг завершено (контекст скасовано після ScanLoopFollow).")
			return // Остаточний вихід, defer flushBuffer() спрацює
		default: // Якщо контекст ще не скасовано
		}

		// Перевірка помилок сканера після завершення ScanLoopFollow
		if errScan := scannerFollow.Err(); errScan != nil {
			if errors.Is(errScan, context.Canceled) {
				logInfo("Сканер зупинено через скасування контексту (після ScanLoopFollow, errScan).")
				return // Виходимо, defer спрацює
			} else if strings.Contains(errScan.Error(), "net/http: request canceled") || errors.Is(errScan, context.DeadlineExceeded) {
				logDebug("Потік логів завершився (таймаут/скасування сервером): %v", errScan)
				// Не показуємо користувачу, просто перепідключимось
			} else {
				logError("Неочікувана помилка сканера: %v", errScan)
				addLineToBuffer(fmt.Sprintf("\nПОМИЛКА ЧИТАННЯ ПОТОКУ: %v\n", errScan))
				// flushBuffer() // Відправиться через defer або на наступній ітерації
			}
			// Пауза перед спробою перепідключення (якщо контекст ще активний)
			if ctx.Err() == nil {
				select {
				case <-time.After(2 * time.Second):
					logDebug("Пауза перед спробою перепідключення до стріму логів...")
					// Цикл for продовжиться
				case <-ctx.Done():
					logInfo("Стрімінг зупинено після помилки сканера під час паузи (контекст скасовано).")
					return // defer спрацює
				}
			} else {
				return
			} // Якщо контекст вже скасовано, виходимо
		} else {
			// Потік завершився без помилки сканера
			// Якщо Follow все ще true і контекст активний, спробуємо перепідключитися
			if followCheck.Checked && ctx.Err() == nil {
				logWarning("Потік логів завершився без помилок сканера. Спроба перепідключення...")
				select {
				case <-time.After(2 * time.Second):
					// Цикл for продовжиться
				case <-ctx.Done():
					logInfo("Стрімінг зупинено після нормального завершення потоку під час паузи (контекст скасовано).")
					return // defer спрацює
				}
			} else {
				// Якщо Follow вимкнено або контекст скасовано, завершуємо
				logInfo("Стрімінг завершено (потік закрився без помилок, Follow=%v, ctx.Err=%v).", followCheck.Checked, ctx.Err())
				return // defer спрацює
			}
		}
	} // Кінець зовнішнього циклу for
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

//func buildErrorDetailsView(resourceType, resourceName string, err error) fyne.CanvasObject {
//	detailsVBox := container.NewVBox()
//	label := widget.NewLabel(fmt.Sprintf("Помилка завантаження деталей для %s '%s':\n%v", resourceType, resourceName, err))
//	label.Wrapping = fyne.TextWrapWord
//	label.Alignment = fyne.TextAlignCenter
//	detailsVBox.Add(label)
//	backButton := widget.NewButton(labelBackToList, func() { displayResourceTableView() })
//	return container.NewBorder(backButton, nil, nil, nil, container.NewPadded(detailsVBox))
//}

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
		werr := watcher.Close()
		if werr != nil {
			logError("Не вдалося зупинити file watcher: %v", werr)
		}
		watcher = nil
		return
	}

	// Додаємо директорію для моніторингу
	// Важливо моніторити саме директорію, бо багато редакторів/інструментів
	// зберігають файл через тимчасовий файл + перейменування.
	err = watcher.Add(watchDir)
	if err != nil {
		logError("Не вдалося додати шлях '%s' до file watcher: %v", watchDir, err)
		werr := watcher.Close()
		if werr != nil {
			logError("Не вдалося зупинити file watcher: %v", werr)
		}
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
				werr := watcher.Close()
				if werr != nil {
					logError("Не вдалося зупинити file watcher: %v", werr)
				}
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

	detailsVBox := container.NewVBox(widget.NewLabelWithStyle("Cluster Overview", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}), widget.NewSeparator())
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

	initializeLoadingRules() // Ініціалізація шляху до kubeconfig
	setupFileWatcher()       // Налаштування моніторингу файлу

	fyneApp = app.NewWithID(appID)
	mainWindow = fyneApp.NewWindow(appTitle)

	// --- Налаштування іконки та системного трея ---
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

	// --- Створення основних віджетів UI ---
	currentContextLabel = widget.NewLabel(labelLoading)
	statusBar = widget.NewLabel("Ініціалізація...")

	// Віджет списку контекстів
	contextListWidget = widget.NewList(
		func() int { stateMu.RLock(); defer stateMu.RUnlock(); return len(allContextNames) },
		func() fyne.CanvasObject { return widget.NewLabel("template context") },
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
		alreadyConnected := connectedContextName
		stateMu.RUnlock()

		if selectedName != "" {
			logInfo("Вибрано контекст у списку: %s", selectedName)
			if selectedName != alreadyConnected {
				go connectLoadAndRefresh(selectedName)
			} else {
				logDebug("Контекст '%s' вже підключений.", selectedName)
				queueUIUpdate(func() { contextListWidget.Unselect(id) })
			}
		}
	}

	// Віджет дерева типів ресурсів
	resourceTypeTree = widget.NewTree(
		func(id widget.TreeNodeID) []widget.TreeNodeID {
			children, _ := resourceTreeData[id]
			sort.Strings(children)
			return children
		},
		func(id widget.TreeNodeID) bool {
			_, isBranch := resourceTreeData[id]
			_, isLeaf := resourceLeafNodes[id]
			return isBranch && !isLeaf
		},
		func(branch bool) fyne.CanvasObject {
			icon := theme.FileTextIcon()
			if branch {
				icon = theme.FolderIcon()
			}
			return container.NewHBox(widget.NewIcon(icon), widget.NewLabel("Template Node"))
		},
		func(id widget.TreeNodeID, branch bool, node fyne.CanvasObject) {
			parts := strings.Split(id, "/")
			displayName := parts[len(parts)-1]
			if cont, ok := node.(*fyne.Container); ok && len(cont.Objects) == 2 {
				if lbl, ok2 := cont.Objects[1].(*widget.Label); ok2 {
					lbl.SetText(displayName)
				}
				if icon, ok2 := cont.Objects[0].(*widget.Icon); ok2 {
					if branch {
						icon.SetResource(theme.FolderIcon())
					} else {
						icon.SetResource(theme.FileTextIcon())
					}
				}
			}
		},
	)
	resourceTypeTree.OnSelected = func(id widget.TreeNodeID) {
		logInfo("Вибрано вузол дерева ресурсів: %s", id)
		isLeaf := resourceLeafNodes[id]

		if isLeaf {
			logInfo("Це листовий вузол - тип ресурсу: %s", id)
			stateMu.Lock()
			currentResType := selectedResourceType
			isConnected := currentClientset != nil
			stateMu.Unlock()

			if !isConnected {
				logWarning("Неможливо завантажити ресурси: не підключено до кластера.")
				queueUIUpdate(func() {
					if statusBar != nil {
						statusBar.SetText("Спочатку виберіть контекст для підключення")
					}
					resourceTypeTree.Unselect(id)
				})
				return
			}

			if id != currentResType {
				stateMu.Lock()
				selectedResourceType = id
				stateMu.Unlock()
				logDebug("Встановлено selectedResourceType = '%s'", id) // Додано лог
				go loadSelectedResources()
			} else {
				logDebug("Тип ресурсу '%s' вже вибрано.", id)
				queueUIUpdate(func() { resourceTypeTree.Unselect(id) })
			}
		} else {
			logDebug("Вибрано групу '%s', розгортаємо/згортаємо.", id)
			queueUIUpdate(func() {
				if resourceTypeTree.IsBranchOpen(id) {
					resourceTypeTree.CloseBranch(id)
				} else {
					resourceTypeTree.OpenBranch(id)
				}
				resourceTypeTree.Unselect(id)
			})
		}
	}
	// Розгортаємо гілки за замовчуванням
	resourceTypeTree.OpenBranch("Cluster")
	resourceTypeTree.OpenBranch("Workloads")
	resourceTypeTree.OpenBranch("Network")
	resourceTypeTree.OpenBranch("Storage")
	resourceTypeTree.OpenBranch("Configuration")
	resourceTypeTree.OpenBranch("Access Control")

	// --- Створення таблиці ресурсів (3-аргументний конструктор) ---
	resourceTable = widget.NewTableWithHeaders(
		// 1. Function to return table size (rows, cols)
		func() (int, int) {
			stateMu.RLock()
			defer stateMu.RUnlock()
			rows := 0
			headers := getHeadersForType(selectedResourceType)
			cols := len(headers)
			if cols == 0 {
				cols = 1
			}

			switch selectedResourceType {
			case "Namespaces":
				rows = len(currentNamespaces)
			case "Nodes":
				rows = len(currentNodes)
			case "Pods":
				rows = len(currentPods)
			case "Deployments":
				rows = len(currentDeployments)
			case "StatefulSets":
				rows = len(currentStatefulSets)
			case "DaemonSets":
				rows = len(currentDaemonSets)
			case "ReplicaSets":
				rows = len(currentReplicaSets)
			case "Jobs":
				rows = len(currentJobs)
			case "CronJobs":
				rows = len(currentCronJobs)
			case "ConfigMaps":
				rows = len(currentConfigMaps)
			case "Secrets":
				rows = len(currentSecrets)
			case "Services":
				rows = len(currentServices)
			case "Ingresses":
				rows = len(currentIngresses)
			case "ServiceAccounts":
				rows = len(currentServiceAccounts)
			case "Roles":
				rows = len(currentRoles)
			case "RoleBindings":
				rows = len(currentRoleBindings)
			case "ClusterRoles":
				rows = len(currentClusterRoles)
			case "ClusterRoleBindings":
				rows = len(currentClusterRoleBindings)
			case "PersistentVolumes":
				rows = len(currentPersistentVolumes)
			case "PersistentVolumeClaims":
				rows = len(currentPersistentVolumeClaims)
			case "StorageClasses":
				rows = len(currentStorageClasses)
			case "System Workloads":
				rows = len(currentSystemWorkloads)
			default:
				rows = 0
			}
			//logDebug("Table Length func: Type='%s', Rows=%d, Cols=%d", selectedResourceType, rows, cols)
			return rows, cols
		},
		// ---> ЗМІНЕНО для діагностики <---
		// 2. Function to create a template DATA cell
		func() fyne.CanvasObject {
			// Використовуємо УНІКАЛЬНИЙ текст для віджета даних
			l := widget.NewLabel("DATA_CELL_WIDGET") // <-- Змінено тут
			l.Truncation = fyne.TextTruncateEllipsis
			return l
		},
		// 3. Function to update a specific DATA cell
		func(id widget.TableCellID, cell fyne.CanvasObject) {
			label, ok := cell.(*widget.Label)
			if !ok {
				return
			}
			label.TextStyle = fyne.TextStyle{}
			label.Alignment = fyne.TextAlignLeading
			stateMu.RLock()
			resType := selectedResourceType
			stateMu.RUnlock()
			dataStr := formatCellData(resType, id.Row, id.Col)
			label.SetText(dataStr)
		},
	) // Кінець 3-аргументного конструктора

	// --- Налаштування заголовків ПІСЛЯ створення таблиці ---
	resourceTable.ShowHeaderRow = true // Вмикаємо рядок заголовків
	resourceTable.ShowHeaderColumn = true

	// ---> ЗМІНЕНО для діагностики <---
	// Створюємо шаблон для клітинки заголовка з УНІКАЛЬНИМ текстом
	resourceTable.CreateHeader = func() fyne.CanvasObject {
		// Використовуємо УНІКАЛЬНИЙ текст для віджета заголовка
		//return widget.NewLabelWithStyle("HEADER_WIDGET_TEXT", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}) // <-- Змінено тут
		return widget.NewLabel("000")
	}

	// Обов'язково: Функція для оновлення ТІЛЬКИ тексту заголовків
	resourceTable.UpdateHeader = func(id widget.TableCellID, template fyne.CanvasObject) {
		label, ok := template.(*widget.Label)
		if !ok {
			logError("Header template was not a *widget.Label")
			return
		}
		stateMu.RLock()
		resType := selectedResourceType
		stateMu.RUnlock()
		headers := getHeadersForType(resType)
		if id.Col == -1 {
			//logDebug("UpdateHeader:id.Col (%d) , id.Row (%d) для заголовків (кількість: %d). Тип ресурсу: %s", id.Col, id.Row, len(headers), resType)
			label.SetText(fmt.Sprintf("%d", id.Row+1))
		} else {
			//logDebug("UpdateHeader:id.Col (%d) , id.Row (%d) для заголовків (кількість: %d). Тип ресурсу: %s", id.Col, id.Row, len(headers), resType)
			label.SetText(headers[id.Col])
		}
		//if id.Col >= 0 && id.Col < len(headers) {
		//	logInfo("UpdateHeader:id.Col (%d) , id.Row (%d) для заголовків (кількість: %d). Тип ресурсу: %s", id.Col, id.Row, len(headers), resType)
		//	label.SetText(headers[id.Col])
		//} else {
		//	logWarning("UpdateHeader: Некоректний id.Col (%d) для заголовків (кількість: %d). Тип ресурсу: %s", id.Col, len(headers), resType)
		//	label.SetText(fmt.Sprintf("HCol %d?", id.Col))
		//}
	}
	// --- Кінець налаштування заголовків ---

	// Встановлення ширини колонок
	resourceTable.SetColumnWidth(0, 250)
	if len(getHeadersForType("Pods")) > 1 {
		resourceTable.SetColumnWidth(1, 250)
	}

	// --- Обробник вибору рядка таблиці ---
	resourceTable.OnSelected = func(id widget.TableCellID) {
		row := id.Row
		logDebug("Вибрано рядок таблиці: %d", row)
		stateMu.RLock()
		resType := selectedResourceType
		var obj interface{}
		var resourceName, resourceNamespace, fullIdentifier string
		validSelection := false

		switch resType { // Логіка отримання об'єкта за рядком
		case "Namespaces":
			if row >= 0 && row < len(currentNamespaces) {
				obj = currentNamespaces[row]
				resourceName = currentNamespaces[row].Name
				validSelection = true
			}
		case "Nodes":
			if row >= 0 && row < len(currentNodes) {
				obj = currentNodes[row]
				resourceName = currentNodes[row].Name
				validSelection = true
			}
		case "Pods":
			if row >= 0 && row < len(currentPods) {
				obj = currentPods[row]
				resourceName = currentPods[row].Name
				resourceNamespace = currentPods[row].Namespace
				validSelection = true
			}
		case "Deployments":
			if row >= 0 && row < len(currentDeployments) {
				obj = currentDeployments[row]
				resourceName = currentDeployments[row].Name
				resourceNamespace = currentDeployments[row].Namespace
				validSelection = true
			}
		case "StatefulSets":
			if row >= 0 && row < len(currentStatefulSets) {
				obj = currentStatefulSets[row]
				resourceName = currentStatefulSets[row].Name
				resourceNamespace = currentStatefulSets[row].Namespace
				validSelection = true
			}
		case "DaemonSets":
			if row >= 0 && row < len(currentDaemonSets) {
				obj = currentDaemonSets[row]
				resourceName = currentDaemonSets[row].Name
				resourceNamespace = currentDaemonSets[row].Namespace
				validSelection = true
			}
		case "ReplicaSets":
			if row >= 0 && row < len(currentReplicaSets) {
				obj = currentReplicaSets[row]
				resourceName = currentReplicaSets[row].Name
				resourceNamespace = currentReplicaSets[row].Namespace
				validSelection = true
			}
		case "Jobs":
			if row >= 0 && row < len(currentJobs) {
				obj = currentJobs[row]
				resourceName = currentJobs[row].Name
				resourceNamespace = currentJobs[row].Namespace
				validSelection = true
			}
		case "CronJobs":
			if row >= 0 && row < len(currentCronJobs) {
				obj = currentCronJobs[row]
				resourceName = currentCronJobs[row].Name
				resourceNamespace = currentCronJobs[row].Namespace
				validSelection = true
			}
		case "ConfigMaps":
			if row >= 0 && row < len(currentConfigMaps) {
				obj = currentConfigMaps[row]
				resourceName = currentConfigMaps[row].Name
				resourceNamespace = currentConfigMaps[row].Namespace
				validSelection = true
			}
		case "Secrets":
			if row >= 0 && row < len(currentSecrets) {
				obj = currentSecrets[row]
				resourceName = currentSecrets[row].Name
				resourceNamespace = currentSecrets[row].Namespace
				validSelection = true
			}
		case "Services":
			if row >= 0 && row < len(currentServices) {
				obj = currentServices[row]
				resourceName = currentServices[row].Name
				resourceNamespace = currentServices[row].Namespace
				validSelection = true
			}
		case "Ingresses":
			if row >= 0 && row < len(currentIngresses) {
				obj = currentIngresses[row]
				resourceName = currentIngresses[row].Name
				resourceNamespace = currentIngresses[row].Namespace
				validSelection = true
			}
		case "ServiceAccounts":
			if row >= 0 && row < len(currentServiceAccounts) {
				obj = currentServiceAccounts[row]
				resourceName = currentServiceAccounts[row].Name
				resourceNamespace = currentServiceAccounts[row].Namespace
				validSelection = true
			}
		case "Roles":
			if row >= 0 && row < len(currentRoles) {
				obj = currentRoles[row]
				resourceName = currentRoles[row].Name
				resourceNamespace = currentRoles[row].Namespace
				validSelection = true
			}
		case "RoleBindings":
			if row >= 0 && row < len(currentRoleBindings) {
				obj = currentRoleBindings[row]
				resourceName = currentRoleBindings[row].Name
				resourceNamespace = currentRoleBindings[row].Namespace
				validSelection = true
			}
		case "ClusterRoles":
			if row >= 0 && row < len(currentClusterRoles) {
				obj = currentClusterRoles[row]
				resourceName = currentClusterRoles[row].Name
				validSelection = true
			}
		case "ClusterRoleBindings":
			if row >= 0 && row < len(currentClusterRoleBindings) {
				obj = currentClusterRoleBindings[row]
				resourceName = currentClusterRoleBindings[row].Name
				validSelection = true
			}
		case "PersistentVolumes":
			if row >= 0 && row < len(currentPersistentVolumes) {
				obj = currentPersistentVolumes[row]
				resourceName = currentPersistentVolumes[row].Name
				validSelection = true
			}
		case "PersistentVolumeClaims":
			if row >= 0 && row < len(currentPersistentVolumeClaims) {
				obj = currentPersistentVolumeClaims[row]
				resourceName = currentPersistentVolumeClaims[row].Name
				resourceNamespace = currentPersistentVolumeClaims[row].Namespace
				validSelection = true
			}
		case "StorageClasses":
			if row >= 0 && row < len(currentStorageClasses) {
				obj = currentStorageClasses[row]
				resourceName = currentStorageClasses[row].Name
				validSelection = true
			}
		case "System Workloads":
			if row >= 0 && row < len(currentSystemWorkloads) {
				fullIdentifier = currentSystemWorkloads[row]
				parts := strings.SplitN(fullIdentifier, "/", 2)
				if len(parts) == 2 {
					resourceName = parts[1]
					resourceNamespace = "kube-system"
					validSelection = true
				} else {
					logError("Неправильний формат ідентифікатора System Workload: %s", fullIdentifier)
					resourceName = fullIdentifier
				}
			}
		default:
			logWarning("Вибрано ресурс невідомого типу '%s' для деталей", resType)
		}
		stateMu.RUnlock()

		if validSelection { // Логіка показу деталей або помилки
			fullName := resourceName
			if resourceNamespace != "" && resType != "Namespaces" && resType != "Nodes" && resType != "PersistentVolumes" && resType != "StorageClasses" && resType != "ClusterRoles" && resType != "ClusterRoleBindings" {
				fullName = resourceNamespace + "/" + fullName
			}
			logInfo("Вибрано ресурс '%s': %s", resType, fullName)
			queueUIUpdate(func() {
				if statusBar != nil {
					statusBar.SetText(fmt.Sprintf("Вибрано %s: %s", resType, fullName))
				}
			})

			if resType == "System Workloads" {
				parts := strings.SplitN(fullIdentifier, "/", 2)
				if len(parts) == 2 {
					go fetchAndDisplayResourceDetails(parts[0], resourceNamespace, resourceName)
				} else {
					queueUIUpdate(func() {
						displayResourceDetails("Error", fmt.Errorf("неправильний формат ідентифікатора '%s'", fullIdentifier))
					})
				}
			} else if obj != nil {
				queueUIUpdate(func() { displayResourceDetails(resType, obj) })
			} else {
				logError("Внутрішня помилка: Об'єкт nil для '%s' %s (Row: %d)", resType, fullName, row)
				queueUIUpdate(func() {
					displayResourceDetails("Error", fmt.Errorf("внутрішня помилка для '%s'", fullName))
				})
			}
		} else {
			logWarning("Не вдалося отримати дані для вибраного рядка типу '%s', Row: %d.", resType, row)
			queueUIUpdate(func() {
				if statusBar != nil {
					statusBar.SetText(fmt.Sprintf("Помилка вибору рядка %d для %s", row, resType))
				}
			})
		}
		queueUIUpdate(func() { resourceTable.Unselect(id) }) // Завжди знімаємо виділення
	}

	// --- Компонування UI ---
	leftPanelContent := container.NewVSplit(
		container.NewBorder(container.NewPadded(widget.NewLabel("Контексти:")), nil, nil, nil, contextListWidget),
		container.NewBorder(container.NewPadded(widget.NewLabel("Ресурси:")), nil, nil, nil, resourceTypeTree),
	)
	leftPanelContent.Offset = 0.4

	rightPanelContainer = container.NewStack(resourceTable) // Починаємо з таблиці

	tappableRightPanel := &tappableContainer{content: rightPanelContainer}
	tappableRightPanel.ExtendBaseWidget(tappableRightPanel)

	split := container.NewHSplit(leftPanelContent, tappableRightPanel)
	split.Offset = 0.3

	mainLayout := container.NewBorder(
		container.NewVBox(currentContextLabel, widget.NewSeparator()),
		statusBar, nil, nil, split,
	)
	mainWindow.SetContent(mainLayout)

	// --- Налаштування вікна та запуск ---
	mainWindow.Resize(fyne.NewSize(1200, 800))
	mainWindow.CenterOnScreen()
	mainWindow.SetCloseIntercept(func() {
		logInfo("Перехоплено закриття вікна...")
		stopFileWatcher()
		fyneApp.Quit()
	})

	go loadAndUpdateState() // Запускаємо початкове завантаження

	mainWindow.ShowAndRun()

	logInfo(logPrefix + " завершено.")
}
