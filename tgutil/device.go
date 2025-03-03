package tgutil

import (
	"github.com/gotd/td/telegram"
)

var Device = telegram.DeviceConfig{ //nolint:exhaustruct
	DeviceModel:    "Desktop",
	SystemVersion:  "Windows 10",
	AppVersion:     "4.2.4 x64",
	LangCode:       "en",
	SystemLangCode: "en-US",
	LangPack:       "tdesktop",
}
