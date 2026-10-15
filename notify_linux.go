package main

import (
	"sync"

	"github.com/godbus/dbus/v5"
)

var notifyConn struct {
	once sync.Once
	conn *dbus.Conn
}

// notify shows a desktop notification via org.freedesktop.Notifications.
// Failures are ignored: notifications are best effort.
func notify(summary, body string) {
	notifyConn.once.Do(func() { notifyConn.conn, _ = dbus.SessionBus() })
	if notifyConn.conn == nil {
		return
	}
	obj := notifyConn.conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications")
	obj.Go("org.freedesktop.Notifications.Notify", 0, nil,
		"fbsync", uint32(0), "folder-sync", summary, body,
		[]string{}, map[string]dbus.Variant{}, int32(-1))
}
