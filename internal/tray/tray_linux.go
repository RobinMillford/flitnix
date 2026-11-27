//go:build linux

package tray

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
)

const (
	sniObjectPath  = dbus.ObjectPath("/StatusNotifierItem")
	menuObjectPath = dbus.ObjectPath("/MenuBar")
	sniInterface   = "org.kde.StatusNotifierItem"
	menuInterface  = "com.canonical.dbusmenu"
	watcherService = "org.kde.StatusNotifierWatcher"
	watcherPath    = dbus.ObjectPath("/StatusNotifierWatcher")
)

var sniInstanceSeq atomic.Uint64

type sniPixmap struct {
	Width  int32
	Height int32
	Data   []byte
}

type sniToolTip struct {
	IconName    string
	IconPixmap  []sniPixmap
	Title       string
	Description string
}

type dbusMenuNode struct {
	ID         int32
	Properties map[string]dbus.Variant
	Children   []dbus.Variant
}

type dbusMenuGroupProp struct {
	ID         int32
	Properties map[string]dbus.Variant
}

type linuxBackend struct {
	mu       sync.Mutex
	conn     *dbus.Conn
	sniProps *prop.Properties
	busName  string

	state      State
	revision   uint32
	rootNode   dbusMenuNode
	nodeByID   map[int32]dbusMenuNode
	actionByID map[int32]string
	onAction   func(string)

	pixmapW int32
	pixmapH int32
	pixmap  []byte
}

func newPlatformBackend(iconPNG []byte) backend {
	w, h, pix := DecodePNGToARGB(iconPNG, 48)
	return &linuxBackend{
		pixmapW:    w,
		pixmapH:    h,
		pixmap:     pix,
		nodeByID:   make(map[int32]dbusMenuNode),
		actionByID: make(map[int32]string),
	}
}

func (b *linuxBackend) Start(initial State, initialMenu []MenuItem, onAction func(id string)) bool {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		return false
	}
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return false
	}

	var hasWatcher bool
	if err := conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, watcherService).Store(&hasWatcher); err != nil || !hasWatcher {
		_ = conn.Close()
		return false
	}

	b.mu.Lock()
	b.conn = conn
	b.state = initial
	b.onAction = onAction
	b.revision = 1
	b.rebuildMenuLocked(initialMenu)
	b.mu.Unlock()

	busName := fmt.Sprintf("org.kde.StatusNotifierItem-%d-%d", os.Getpid(), sniInstanceSeq.Add(1))
	reply, err := conn.RequestName(busName, dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		_ = conn.Close()
		return false
	}

	sniObj := &sniHandler{b: b}
	menuObj := &menuHandler{b: b}
	if err := conn.Export(sniObj, sniObjectPath, sniInterface); err != nil {
		_ = conn.Close()
		return false
	}
	if err := conn.Export(menuObj, menuObjectPath, menuInterface); err != nil {
		_ = conn.Close()
		return false
	}

	// Introspection helps desktop shell watchers discover properties cleanly.
	_ = conn.Export(introspect.NewIntrospectable(&introspect.Node{
		Name: string(sniObjectPath),
		Interfaces: []introspect.Interface{
			introspect.IntrospectData,
			prop.IntrospectData,
			{Name: sniInterface},
		},
	}), sniObjectPath, "org.freedesktop.DBus.Introspectable")

	_ = conn.Export(introspect.NewIntrospectable(&introspect.Node{
		Name: string(menuObjectPath),
		Interfaces: []introspect.Interface{
			introspect.IntrospectData,
			prop.IntrospectData,
			{Name: menuInterface},
		},
	}), menuObjectPath, "org.freedesktop.DBus.Introspectable")

	pixmaps := b.pixmaps()
	sniPropSpec := map[string]map[string]*prop.Prop{
		sniInterface: {
			"Category":   {Value: "ApplicationStatus", Writable: false, Emit: prop.EmitTrue},
			"Id":         {Value: "flitnix", Writable: false, Emit: prop.EmitTrue},
			"Title":      {Value: "Flitnix", Writable: false, Emit: prop.EmitTrue},
			"Status":     {Value: statusString(initial), Writable: false, Emit: prop.EmitTrue},
			"WindowId":   {Value: uint32(0), Writable: false, Emit: prop.EmitTrue},
			"IconName":   {Value: "flitnix", Writable: false, Emit: prop.EmitTrue},
			"IconPixmap": {Value: pixmaps, Writable: false, Emit: prop.EmitTrue},
			"ItemIsMenu": {Value: false, Writable: false, Emit: prop.EmitTrue},
			"Menu":       {Value: menuObjectPath, Writable: false, Emit: prop.EmitTrue},
			"ToolTip":    {Value: sniToolTip{Title: "Flitnix", Description: initial.Tooltip}, Writable: false, Emit: prop.EmitTrue},
		},
	}
	sniProps, err := prop.Export(conn, sniObjectPath, sniPropSpec)
	if err != nil {
		_ = conn.Close()
		return false
	}

	menuPropSpec := map[string]map[string]*prop.Prop{
		menuInterface: {
			"Version":       {Value: uint32(3), Writable: false, Emit: prop.EmitFalse},
			"TextDirection": {Value: "ltr", Writable: false, Emit: prop.EmitFalse},
			"Status":        {Value: "normal", Writable: false, Emit: prop.EmitFalse},
		},
	}
	if _, err := prop.Export(conn, menuObjectPath, menuPropSpec); err != nil {
		_ = conn.Close()
		return false
	}

	b.mu.Lock()
	b.sniProps = sniProps
	b.busName = busName
	b.mu.Unlock()

	if call := conn.Object(watcherService, watcherPath).Call(watcherService+".RegisterStatusNotifierItem", 0, busName); call.Err != nil {
		// Fall back to registering the object path (older watchers expect path vs bus name).
		_ = conn.Object(watcherService, watcherPath).Call(watcherService+".RegisterStatusNotifierItem", 0, string(sniObjectPath))
	}

	return true
}

func (b *linuxBackend) Update(state State, menu []MenuItem) {
	b.mu.Lock()
	conn := b.conn
	props := b.sniProps
	if conn == nil {
		b.mu.Unlock()
		return
	}
	b.state = state
	b.revision++
	rev := b.revision
	b.rebuildMenuLocked(menu)
	b.mu.Unlock()

	st := statusString(state)
	if props != nil {
		props.SetMust(sniInterface, "Status", st)
		props.SetMust(sniInterface, "ToolTip", sniToolTip{Title: "Flitnix", Description: state.Tooltip})
	}
	_ = conn.Emit(sniObjectPath, sniInterface+".NewStatus", st)
	_ = conn.Emit(sniObjectPath, sniInterface+".NewToolTip")
	_ = conn.Emit(menuObjectPath, menuInterface+".LayoutUpdated", rev, int32(0))
}

func (b *linuxBackend) Stop() {
	b.mu.Lock()
	conn := b.conn
	b.conn = nil
	b.sniProps = nil
	b.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (b *linuxBackend) pixmaps() []sniPixmap {
	if b.pixmapW > 0 && b.pixmapH > 0 && len(b.pixmap) > 0 {
		return []sniPixmap{{Width: b.pixmapW, Height: b.pixmapH, Data: b.pixmap}}
	}
	return nil
}

func statusString(st State) string {
	if st.Attention || st.PendingCount > 0 {
		return "NeedsAttention"
	}
	return "Active"
}

func (b *linuxBackend) rebuildMenuLocked(items []MenuItem) {
	b.nodeByID = make(map[int32]dbusMenuNode)
	b.actionByID = make(map[int32]string)
	nextID := int32(1)

	var buildChildren func(list []MenuItem) []dbus.Variant
	buildChildren = func(list []MenuItem) []dbus.Variant {
		out := make([]dbus.Variant, 0, len(list))
		for _, item := range list {
			id := nextID
			nextID++
			props := map[string]dbus.Variant{
				"visible": dbus.MakeVariant(item.Visible),
				"enabled": dbus.MakeVariant(item.Enabled),
			}
			var children []dbus.Variant
			if item.IsSeparator {
				props["type"] = dbus.MakeVariant("separator")
			} else {
				props["label"] = dbus.MakeVariant(item.Label)
				if item.Checkable {
					if item.Radio {
						props["toggle-type"] = dbus.MakeVariant("radio")
					} else {
						props["toggle-type"] = dbus.MakeVariant("checkmark")
					}
					val := int32(0)
					if item.Checked {
						val = 1
					}
					props["toggle-state"] = dbus.MakeVariant(val)
				}
				if len(item.Children) > 0 {
					props["children-display"] = dbus.MakeVariant("submenu")
					children = buildChildren(item.Children)
				}
				if item.ID != "" {
					b.actionByID[id] = item.ID
				}
			}
			node := dbusMenuNode{ID: id, Properties: props, Children: children}
			b.nodeByID[id] = node
			out = append(out, dbus.MakeVariant(node))
		}
		return out
	}

	root := dbusMenuNode{
		ID: 0,
		Properties: map[string]dbus.Variant{
			"children-display": dbus.MakeVariant("submenu"),
		},
		Children: buildChildren(items),
	}
	b.rootNode = root
	b.nodeByID[0] = root
}

type sniHandler struct {
	b *linuxBackend
}

func (h *sniHandler) Activate(_, _ int32) *dbus.Error {
	h.b.mu.Lock()
	cb := h.b.onAction
	h.b.mu.Unlock()
	if cb != nil {
		go cb(ActionToggleWindow)
	}
	return nil
}

func (h *sniHandler) SecondaryActivate(_, _ int32) *dbus.Error {
	return h.Activate(0, 0)
}

func (h *sniHandler) ContextMenu(_, _ int32) *dbus.Error {
	return nil
}

func (h *sniHandler) Scroll(_ int32, _ string) *dbus.Error {
	return nil
}

type menuHandler struct {
	b *linuxBackend
}

func (h *menuHandler) GetLayout(parentID int32, recursionDepth int32, _ []string) (uint32, dbusMenuNode, *dbus.Error) {
	h.b.mu.Lock()
	rev := h.b.revision
	node, ok := h.b.nodeByID[parentID]
	if !ok {
		node = h.b.rootNode
	}
	h.b.mu.Unlock()

	if recursionDepth == 0 {
		node.Children = nil
	}
	return rev, node, nil
}

func (h *menuHandler) GetGroupProperties(ids []int32, _ []string) ([]dbusMenuGroupProp, *dbus.Error) {
	h.b.mu.Lock()
	defer h.b.mu.Unlock()
	out := make([]dbusMenuGroupProp, 0, len(ids))
	for _, id := range ids {
		if node, ok := h.b.nodeByID[id]; ok {
			out = append(out, dbusMenuGroupProp{ID: id, Properties: node.Properties})
		}
	}
	return out, nil
}

func (h *menuHandler) GetProperty(id int32, name string) (dbus.Variant, *dbus.Error) {
	h.b.mu.Lock()
	defer h.b.mu.Unlock()
	if node, ok := h.b.nodeByID[id]; ok {
		if v, exists := node.Properties[name]; exists {
			return v, nil
		}
	}
	return dbus.MakeVariant(""), nil
}

func (h *menuHandler) Event(id int32, eventID string, _ dbus.Variant, _ uint32) *dbus.Error {
	if eventID != "clicked" {
		return nil
	}
	h.b.mu.Lock()
	act := h.b.actionByID[id]
	cb := h.b.onAction
	h.b.mu.Unlock()
	if act != "" && cb != nil {
		go cb(act)
	}
	return nil
}

func (h *menuHandler) AboutToShow(_ int32) (bool, *dbus.Error) {
	return false, nil
}
