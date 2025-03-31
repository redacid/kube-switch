package main

import (
	"fmt"
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"
	"github.com/fsnotify/fsnotify"
	"github.com/gen2brain/beeep"
	"github.com/redacid/kube-switch/pkg/resdata"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"log"
	"sort"
)

type useContextOptions struct {
	configAccess clientcmd.ConfigAccess
	contextName  string
}

var (
	cfg            *clientcmdapi.Config
	contextData    []string
	currentContext string
	originFilename string
	pathOptions    = clientcmd.NewDefaultPathOptions()
	options        = &useContextOptions{configAccess: pathOptions}
	myApp          = app.NewWithID("redacid.k8s.context")
	desk           = myApp.(desktop.App)
	myWindow       = myApp.NewWindow("K8S Contexts")
	hbox           *fyne.Container
	list           *widget.List
	btnSetContext  *widget.Button
	menu           = fyne.NewMenu("Systray menu")
)

func readConfig(from string) {
	//cfg = clientcmd.GetConfigFromFileOrDie(cfgFile)
	cfg, _ = options.configAccess.GetStartingConfig()
	currentContext = cfg.CurrentContext
	originFilename = options.configAccess.GetDefaultFilename()
	contextData = nil
	for k := range cfg.Contexts {
		contextData = append(contextData, k)
	}
	sort.Strings(contextData)
	log.Printf("readConfig From: %s", from)
	//printConfigData()
}

func refreshList(from string) {
	menu.Items = nil
	setTrayMenu("refeshList")
	desk.SetSystemTrayMenu(menu)

	btnSetContext.Hide()
	list.Refresh()
	for i := 0; i < list.Length(); i++ {
		list.RefreshItem(i)
	}
	hbox.Refresh()
	log.Printf("refreshList From: %s", from)
}

func sendNotification(t string, m string) {
	err := beeep.Notify(t, m, "")
	if err != nil {
		panic(err)
	}
}

func setCurrentContext(contextName string) {
	cfgNew, _ := options.configAccess.GetStartingConfig()
	cfgNew.CurrentContext = contextName
	err := clientcmd.ModifyConfig(options.configAccess, *cfgNew, true)
	if err != nil {
		log.Printf("Error change context to %v: %v\n ", contextName, err)
	} else {
		log.Printf("Set Current Context: %v\n", contextName)
	}
	readConfig("setCurrentContext")
	refreshList("setCurrentContext")
}

func setTrayMenu(from string) {
	var menuItem *fyne.MenuItem = nil
	var menuItems []*fyne.MenuItem = nil
	menuItem = fyne.NewMenuItem("Open main window", func() { myWindow.Show() })
	menuItems = append(menuItems, menuItem)
	menuItem = fyne.NewMenuItemSeparator()
	menuItems = append(menuItems, menuItem)

	for _, k := range contextData {
		menuItem = fyne.NewMenuItem(k, func() { setCurrentContext(k) })
		if k == cfg.CurrentContext {
			//menuItem.Icon = icoClusterCurrent
			menuItem.Icon = resdata.IcoClusterCurrent
		} else {
			menuItem.Icon = resdata.IcoCluster
		}
		menuItems = append(menuItems, menuItem)
	}
	menuItem = fyne.NewMenuItemSeparator()
	menuItems = append(menuItems, menuItem)
	menuItem = fyne.NewMenuItem("Quit", func() { myApp.Quit() })
	menuItems = append(menuItems, menuItem)
	//menu = fyne.NewMenu("Systray menu")
	menu.Items = menuItems
	log.Printf("setTrayMenu From: %v\n", from)
}

func makeListTab() fyne.CanvasObject {
	icon := widget.NewIcon(nil)
	label := widget.NewLabel("")
	btnSetContext = widget.NewButton("Set Context", func() {})
	btnSetContext.Hide()
	hbox = container.NewHBox(label, btnSetContext)
	list = widget.NewList(
		func() int {
			return len(contextData)
		},
		func() fyne.CanvasObject {
			return container.NewHBox(
				widget.NewIcon(resdata.IcoChecked),
				widget.NewLabel(""))
		},
		func(id widget.ListItemID, item fyne.CanvasObject) {
			if contextData[id] == currentContext {
				item.(*fyne.Container).Objects[0].(*widget.Icon).Show()
				item.(*fyne.Container).Objects[1].(*widget.Label).SetText(contextData[id])
			} else {
				item.(*fyne.Container).Objects[0].(*widget.Icon).Hide()
				item.(*fyne.Container).Objects[1].(*widget.Label).SetText(contextData[id])
			}
		},
	)
	list.OnSelected = func(id widget.ListItemID) {
		btnSetContext.Hide()
		if contextData[id] != currentContext {
			btnSetContext.SetText("Use Context " + contextData[id])
			btnSetContext.OnTapped = func() {
				setCurrentContext(contextData[id])
			}
			btnSetContext.Show()
		}
	}
	list.OnUnselected = func(id widget.ListItemID) {
		label.SetText("")
		icon.SetResource(nil)
	}
	list.SetItemHeight(5, 50)
	for i := 0; i < list.Length(); i++ {
		list.RefreshItem(i)
	}
	list.Refresh()
	hbox.Refresh()
	return container.NewHSplit(list, container.NewCenter(hbox))
}

func myWatcher() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Println("ERROR", err)
	}
	defer func() {
		err := watcher.Close()
		if err != nil {
			log.Println("ERROR", err)
		}
	}()

	done := make(chan bool)
	go func() {
		for {
			select {
			case event := <-watcher.Events:
				log.Printf("Watch event: %s\n", event.Op.String())
				if event.Op.Has(fsnotify.Write) {
					readConfig("Goroutine")
					refreshList("Goroutine")
					sendNotification("Context changed", "Current context set to "+cfg.CurrentContext)
				}

			case err := <-watcher.Errors:
				log.Println("Watch ERROR", err)
			}
		}
	}()
	if err := watcher.Add(originFilename); err != nil {
		log.Println("Watch ERROR", err)
	}
	<-done
}

func makeMainMenu() {
	menu := fyne.NewMenu("File")
	mMenu := fyne.NewMainMenu(menu)
	myWindow.SetMainMenu(mMenu)
}

func makeMainWindow() {
	myWindow.Resize(fyne.NewSize(800, 350))
	myWindow.SetFixedSize(true)
	//myWindow.CenterOnScreen()
	myWindow.SetIcon(resdata.IcoGreen)
	myWindow.SetContent(makeListTab())
	myWindow.SetCloseIntercept(func() {
		myWindow.Hide()
	})
	myWindow.Show()

}

func printConfigData() {
	fmt.Printf("Current context: %v\n", cfg.CurrentContext)
	fmt.Printf("Origin Filename: %v\n", originFilename)
	for k := range cfg.Contexts {
		fmt.Printf("Context: %v\n", k)
		fmt.Printf("AuthInfo: %v\n", cfg.Contexts[k].AuthInfo)
		fmt.Printf("Kubeconf: %v\n", cfg.Contexts[k].LocationOfOrigin)
		fmt.Printf("Cluster: %v\n", cfg.Contexts[k].Cluster)
		fmt.Printf("Namespace: %v\n", cfg.Contexts[k].Namespace)
		for ek, ev := range cfg.Contexts[k].Extensions {
			fmt.Printf("Extension %v: %v\n", ek, ev)
		}
		fmt.Println("-----------------------------------------------------------------")
	}

	for kc := range cfg.Clusters {
		fmt.Printf("Cluster: %v\n", kc)
		fmt.Printf("Server: %v\n", cfg.Clusters[kc].Server)
		fmt.Printf("CertificateAuthority: %v\n", cfg.Clusters[kc].CertificateAuthority)
		fmt.Printf("TLSServerName: %v\n", cfg.Clusters[kc].TLSServerName)
		fmt.Printf("LocationOfOrigin: %v\n", cfg.Clusters[kc].LocationOfOrigin)
		for ek, ev := range cfg.Clusters[kc].Extensions {
			fmt.Printf("Extension %v: %v\n", ek, ev)
		}
		fmt.Println("==================================================================")
	}

}

func main() {
	readConfig("Main")
	makeMainWindow()
	refreshList("Main")
	makeMainMenu()
	go myWatcher()
	myApp.SetIcon(resdata.IcoGreen)
	myApp.Run()
}
