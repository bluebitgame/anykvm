# Automatic startup and services

Keep the executable and runtime libraries in a permanent location, then:

```sh
anykvm -service install -config /absolute/path/to/config.yaml
anykvm -service status
anykvm -service restart
anykvm -service uninstall
```

Omit `-config` to use the default beside the executable; see
[configuration files](configuration.md#configuration-files-and-device-discovery).
Installation records executable/configuration/working-directory paths, starts
the service and updates an existing installation. Uninstall retains
configuration, certificates, pairing state and logs.

Stop a background instance before starting a console instance with the same
ports/devices. Service actions cannot perform interactive pairing; complete
[GameStream pairing](gamestream.md#pairing-and-access) in a terminal first.

## macOS

Install as the logged-in user without sudo. A LaunchAgent runs after GUI login;
grant capture/input permissions to the executable.

Logs: `~/Library/Logs/AnyKVM/`.

## Linux

Install as the desktop user on systemd. The user service follows
`graphical-session.target`; the desktop must provide its environment. This does
not create a headless desktop.

Logs: `journalctl --user -u anykvm.service`.

## Windows

Install from an Administrator terminal. The LocalSystem service supervises a
worker in the active console session. Lock/UAC/user-switching behavior still
needs validation on the deployment host.

Logs: `%ProgramData%\AnyKVM\logs\service.log`.
