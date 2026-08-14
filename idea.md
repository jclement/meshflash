I want a Golang based app (Win, Mac, Lin) that can flash devices with either MeshCore or Meshtastic.

so

github.com/jclement/meshflash (public)
GHA to build

meshflash upgrade - downloads and updates to lateset meshflash
meshflash update - downloads latest images for various devices
meshflash doctor - list connected devices and images
meshflash configure - TUI to choose devices to support
meshflash flash - choose a connected devices and firmware, and flashes it.

Intended to be installed on an offline device (maybe an RPI)? for field flashing
Toughbook or seomthing

You'll probably need to look at the meshcore/meshtastic flasher for device lists, and flash approach.