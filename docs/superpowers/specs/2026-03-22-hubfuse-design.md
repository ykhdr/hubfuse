# HubFuse Design Spec

## Overview

HubFuse — инструмент для объединения устройств в локальной сети с прозрачным файловым доступом. Позволяет монтировать папки с других устройств как обычные локальные директории через SSHFS. Работает как демон в фоне, автоматически реагируя на появление и исчезновение устройств.

**Язык:** Go
**Платформы:** Linux, macOS
**Интерфейс:** CLI
**Версия протокола:** 1

## Components

### hubfuse-hub (сервер)

Выделенный сервер, выполняющий роль реестра и координатора сети.

Ответственность:
- gRPC API для клиентов (mTLS)
- Реестр устройств (nickname, device_id, текущий IP, статус online/offline)
- Кэш расшаренных папок (что доступно и с какими правами)
- Генерация invite-кодов для первичного знакомства устройств
- Отслеживание heartbeat, пометка устройств как offline при таймауте

Хаб **не хранит секретов** — только метаданные о сети.

### hubfuse-agent (клиент-демон)

Демон, работающий на каждом устройстве в сети.

Ответственность:
- Регистрация на хабе при запуске (пуш своих шар)
- Приём обновлений от хаба через gRPC stream (кто появился/ушёл)
- Монтирование SSHFS при появлении нужных устройств
- Размонтирование при исчезновении
- Управление SSH-ключами (генерация, хранение)
- Локальный SSH-сервер для входящих SSHFS-подключений (порт по умолчанию: 2222)

## CLI

### hubfuse-hub

```
hubfuse-hub start       # запуск хаба
hubfuse-hub stop        # остановка хаба (рассылает DeviceOffline всем, затем завершается)
hubfuse-hub status      # статус хаба
```

### hubfuse-agent

```
# Жизненный цикл
hubfuse-agent start                     # запуск демона
hubfuse-agent stop                      # остановка (размонтирует все шары, уведомляет хаб, завершается)
hubfuse-agent status                    # статус демона, активные монтирования
hubfuse-agent join <hub-address>        # первичная регистрация в сети (см. секцию Join Flow)
hubfuse-agent pair <device>             # инициация знакомства (invite-код)
hubfuse-agent devices                   # список устройств в сети
hubfuse-agent rename <new-nickname>     # смена nickname устройства

# Управление шарами
hubfuse-agent share add <path> --alias <name> --permissions <ro|rw> --allow <devices>
hubfuse-agent share remove <alias>
hubfuse-agent share list

# Управление монтированиями
hubfuse-agent mount add <device>:<share> --to <local-path>
hubfuse-agent mount remove <device>:<share>
hubfuse-agent mount list
```

Форматы прав: `ro` (алиас: `read-only`), `rw` (алиас: `read-write`) — оба допустимы в CLI и конфиге.

## Configuration

Формат: **KDL** (парсер: `github.com/ykhdr/kdl-config`).

Файл конфига: `~/.hubfuse/config.kdl`

```kdl
device {
    nickname "laptop"
}

hub {
    address "192.168.1.100:9090"
}

agent {
    ssh-port 2222
}

shares {
    share "/home/user/projects" alias="projects" permissions="rw" {
        allowed-devices "desktop" "tablet"
    }
    share "/home/user/photos" alias="photos" permissions="ro" {
        allowed-devices "all"
    }
}

mounts {
    mount device="desktop" share="projects" to="~/remote/desktop-projects"
    mount device="nas" share="media" to="~/remote/media"
}
```

Пути в конфиге поддерживают `~` — агент раскрывает их в домашнюю директорию пользователя при парсинге.

Source of truth — конфиг на устройстве. Хаб хранит кэш текущего состояния. При запуске агент пушит свои шары на хаб.

**Hot-reload:** агент отслеживает изменения `config.kdl` через filesystem notifications (fsnotify). При изменении — перечитывает конфиг, вычисляет diff с текущим состоянием, и вызывает `UpdateShares` на хабе если шары изменились. Новые монтирования применяются автоматически.

## Communication (gRPC API)

Хаб предоставляет единый gRPC-сервис. Все вызовы аутентифицированы через mTLS (см. секцию Security).

### Типы данных

```protobuf
message Share {
    string alias = 1;
    string permissions = 2;        // "ro" | "rw"
    repeated string allowed_devices = 3;  // device_id или "all"
}
// Примечание: локальный путь к папке НЕ передаётся по gRPC —
// удалённые устройства знают только alias.
```

### Join Flow

`hubfuse-agent join <hub-address>` — одноразовая операция первичной регистрации:

1. Агент генерирует `device_id` (UUID v4) и сохраняет в `~/.hubfuse/device.json`
2. Агент запрашивает у пользователя nickname
3. Агент подключается к хабу без mTLS (первый вызов — единственный неаутентифицированный)
4. Агент вызывает `Join(device_id, nickname)` — специальный RPC для первичной регистрации
5. Хаб проверяет уникальность nickname. Если занят — возвращает ошибку, агент просит другой nickname
6. Хаб генерирует клиентский TLS-сертификат (подписанный CA хаба) с device_id в CN и возвращает его вместе с CA-сертификатом хаба
7. Агент сохраняет: клиентский TLS-сертификат и ключ в `~/.hubfuse/tls/`, CA-сертификат в `~/.hubfuse/tls/ca.crt`, hub-address в `~/.hubfuse/config.kdl`

При последующих запусках (`hubfuse-agent start`) агент использует сохранённый TLS-сертификат для mTLS-подключения к хабу и вызывает `Register` — хаб извлекает device_id из сертификата и обновляет IP/статус.

**Ошибки при Register:**
- Nickname занят другим device_id → `error: nickname_taken`
- Неизвестный protocol_version → `error: unsupported_protocol_version`
- При переподключении nickname изменить нельзя (nickname фиксируется при `Join`). Для смены nickname — отдельная команда `hubfuse-agent rename <new-nickname>` (вызывает `Rename` RPC).

### Регистрация и жизненный цикл

- `Join(device_id, nickname) → JoinResponse {success, error, client_cert, client_key, ca_cert}` — первичная регистрация (единственный неаутентифицированный RPC). Хаб проверяет уникальность nickname, генерирует TLS-сертификат.
- `Register(shares[], ssh_port, protocol_version) → RegisterResponse {success, error, devices_online[]}` — клиент регистрируется при каждом запуске. device_id извлекается из mTLS-сертификата. Хаб обновляет IP, статус, шары. В ответе — текущий список онлайн-устройств.
- `Rename(new_nickname) → RenameResponse {success, error}` — смена nickname. Хаб проверяет уникальность нового имени.
- `Heartbeat() → HeartbeatResponse {success}` — периодический пинг (device_id из mTLS-сертификата). Интервал: **10 секунд**. Таймаут offline: **30 секунд** (3 пропущенных heartbeat).
- `UpdateShares(shares[]) → UpdateSharesResponse {success}` — клиент обновляет список своих шар (device_id из mTLS-сертификата).
- `Deregister() → DeregisterResponse {success}` — клиент корректно отключается при `agent stop` (device_id из mTLS-сертификата).

### Подписка на события (server stream)

- `Subscribe(device_id) → stream Event` — после регистрации клиент открывает стрим и получает события:
  - `DeviceOnline {device_id, nickname, ip, ssh_port, shares[]}` — устройство появилось
  - `DeviceOffline {device_id, nickname}` — устройство пропало
  - `SharesUpdated {device_id, shares[]}` — устройство обновило свои шары
  - `PairingRequested {from_device_id, from_nickname}` — запрос на pairing от другого устройства
  - `PairingCompleted {peer_device_id, peer_public_key}` — pairing подтверждён, ключ партнёра доставлен

### Pairing (invite-код)

- `RequestPairing(to_device, public_key) → RequestPairingResponse {invite_code}` — инициировать знакомство. from_device извлекается из mTLS. Инициатор передаёт свой публичный SSH-ключ. Хаб генерирует invite-код и отправляет `PairingRequested` целевому устройству через Subscribe stream.
- `ConfirmPairing(device_id, invite_code, public_key) → ConfirmPairingResponse {success, peer_public_key}` — второе устройство подтверждает код и отправляет свой публичный SSH-ключ. В ответе получает публичный ключ инициатора. Инициатору также доставляется публичный ключ подтверждающего через `PairingCompleted` событие.
  - `PairingCompleted {peer_device_id, peer_public_key}` — дополнительное событие в Subscribe stream.

**Обмен ключами при pairing:**

1. Устройство A вызывает `RequestPairing(B, A_public_key)` → получает invite-код
2. Устройство B получает событие `PairingRequested {from: A}` → показывает запрос на ввод кода
3. Пользователь вводит invite-код на устройстве B
4. Устройство B вызывает `ConfirmPairing(B, code, B_public_key)` → получает `A_public_key` в ответе
5. Устройство A получает событие `PairingCompleted {peer: B, B_public_key}`
6. Оба устройства сохраняют полученные публичные ключи в `~/.hubfuse/known_devices/`

**Примечание:** публичные SSH-ключи не являются секретами. Их передача через хаб безопасна — хаб выступает курьером, но не может использовать эти ключи для доступа к устройствам. Приватные ключи никогда не покидают устройства.

Файловый трафик (SSHFS) идёт мимо хаба, напрямую между клиентами по SSH.

## Lifecycle Scenarios

### Первый запуск (настройка сети)

1. Пользователь запускает хаб: `hubfuse-hub start`
2. На каждом устройстве: `hubfuse-agent join 192.168.1.100:9090` — агент генерирует device_id (UUID v4), пользователь задаёт nickname (уникальный в пределах хаба), агент регистрируется
3. Пользователь настраивает шары и монтирования через CLI или конфиг

### Первое подключение между устройствами (pairing)

1. Laptop хочет смонтировать `projects` с desktop
2. `hubfuse-agent pair desktop` → хаб генерирует invite-код `HUB-7K2-MNX`, показывает на laptop
3. На desktop через Subscribe stream приходит `PairingRequested` → демон показывает запрос на ввод кода
4. Пользователь вводит `HUB-7K2-MNX` на desktop
5. Desktop вызывает `ConfirmPairing` с кодом и своим публичным ключом → получает публичный ключ laptop
6. Laptop получает `PairingCompleted` с публичным ключом desktop
7. Оба сохраняют ключи в `~/.hubfuse/known_devices/`
8. Pairing запоминается — повторно не нужен

### Обычная работа (демон запущен)

1. Агент стартует, подключается к хабу, вызывает `Register` со своими шарами
2. В `RegisterResponse` получает список онлайн-устройств
3. Открывает gRPC stream (`Subscribe`) для получения событий
4. Для каждого устройства с pairing и конфигом монтирования — автоматически монтирует SSHFS
5. Heartbeat каждые 10 секунд
6. `DeviceOnline` → монтирует, если есть конфиг и pairing
7. `DeviceOffline` → размонтирует

### Потеря связи

1. Устройство переключилось на другую сеть или потеряло связь
2. Хаб не получает heartbeat — через 30 секунд помечает устройство как offline
3. Хаб рассылает `DeviceOffline` всем подписчикам
4. Остальные агенты размонтируют папки этого устройства
5. При возвращении в сеть — агент переподключается к хабу, цикл повторяется

### Переподключение и восстановление

- **Агент не может подключиться к хабу при старте:** повторные попытки с exponential backoff (1s, 2s, 4s, ..., max 60s). Логирует предупреждения.
- **gRPC stream разрывается:** агент переподключается с backoff и вызывает `Register` повторно (хаб отдаст текущее состояние). Активные SSHFS-монтирования сохраняются — они работают напрямую между устройствами и не зависят от хаба.
- **Хаб перезапускается:** все стримы разрываются. Агенты переподключаются и регистрируются заново. Хаб восстанавливает состояние из SQLite (pairings, devices) и из `Register`-вызовов агентов (текущие шары, IP).

### Graceful Shutdown

- **`hubfuse-agent stop`:** агент размонтирует все активные SSHFS-монтирования, вызывает `Deregister` на хабе, завершается.
- **`hubfuse-hub stop`:** хаб рассылает `DeviceOffline` для всех устройств всем подписчикам, закрывает все gRPC-стримы, завершается.

## Storage

### На хабе — SQLite

Таблицы:
- `devices` — device_id (UUID v4, PK), nickname (UNIQUE), last_ip, ssh_port, status, last_heartbeat
- `shares` — device_id (FK), alias, permissions, allowed_devices
- `pairings` — device_a (FK), device_b (FK), paired_at
- `pending_invites` — invite_code, from_device, to_device, from_public_key, expires_at (TTL: 5 минут), attempts (default: 0, max: 5)

### На клиенте — файлы

- `~/.hubfuse/config.kdl` — шары, монтирования, адрес хаба, SSH-порт
- `~/.hubfuse/device.json` — device_id (UUID v4), nickname
- `~/.hubfuse/tls/` — клиентский TLS-сертификат, ключ, CA-сертификат хаба
- `~/.hubfuse/keys/` — SSH-ключи hubfuse (приватный + публичный)
- `~/.hubfuse/known_devices/` — публичные ключи paired-устройств (по device_id)

## Security

- **gRPC аутентификация — mTLS:** при `join` хаб генерирует клиентский TLS-сертификат (с device_id в CN), подписанный CA хаба, и возвращает его агенту вместе с CA-сертификатом. Агент сохраняет сертификат в `~/.hubfuse/tls/` и использует для всех последующих gRPC-вызовов. Хаб верифицирует клиентский сертификат и извлекает device_id. `Join` — единственный неаутентифицированный RPC. Без валидного сертификата невозможно вызвать ни один другой RPC.
- **Хаб не хранит секретов** — только метаданные и публичные ключи. Приватные ключи (TLS и SSH) никогда не покидают устройство.
- **Pairing через invite-код** — формат: фиксированный префикс `HUB` + 2 группы по 3 случайных символа (A-Z, 0-9), разделённых дефисом, например `HUB-7K2-MNX` (~31 бит энтропии). Одноразовый, TTL 5 минут. Максимум 5 попыток ввода — после чего invite аннулируется. Публичные SSH-ключи передаются через хаб (они не являются секретами). После pairing устройства подключаются друг к другу напрямую по SSH.
- **SSHFS-соединения** — аутентификация по SSH-ключам. Агент-владелец шары проверяет подключающийся публичный ключ по списку в `known_devices/` и сверяет device_id подключающегося с `allowed-devices` из конфига шары.
- **Идентификация устройств** — device_id (UUID v4) + nickname (уникальный в пределах хаба). Даже при смене IP устройство узнаётся по device_id.
- **Никнейм уникален** в пределах хаба. Проверяется при `Join` (первичная регистрация) и при `Rename`. Если nickname занят — операция отклоняется.

## Logging

Структурированные логи в stderr (по умолчанию) или в файл `~/.hubfuse/hubfuse-agent.log` / `~/.hubfuse-hub/hubfuse-hub.log`.

Уровни: `debug`, `info`, `warn`, `error`. По умолчанию: `info`.

## Project Structure

```
hubfuse/
├── cmd/
│   ├── hubfuse-hub/
│   │   └── main.go
│   └── hubfuse-agent/
│       └── main.go
├── proto/
│   └── hubfuse.proto
├── internal/
│   ├── hub/
│   │   ├── server.go         # gRPC-сервер хаба
│   │   ├── registry.go       # реестр устройств, heartbeat
│   │   ├── pairing.go        # invite-коды, подтверждение
│   │   └── store/            # SQLite-хранилище
│   ├── agent/
│   │   ├── daemon.go         # демон: подключение к хабу, event loop
│   │   ├── mounter.go        # монтирование/размонтирование SSHFS
│   │   ├── keys.go           # генерация и управление SSH-ключами
│   │   └── config/           # парсинг KDL-конфига
│   └── common/               # общие типы, утилиты
├── go.mod
├── go.sum
└── README.md
```

### Зависимости

- `google.golang.org/grpc` — gRPC
- `modernc.org/sqlite` — SQLite (pure Go, без CGO — упрощает кросс-компиляцию)
- `github.com/ykhdr/kdl-config` — KDL-парсер
- `github.com/spf13/cobra` — CLI

## Design Decisions

| Решение | Выбор | Причина |
|---------|-------|---------|
| Обнаружение устройств | Heartbeat к хабу (10s интервал, 30s таймаут) | Проще, чем mDNS; хаб выделенный |
| Протокол хаб-клиент | gRPC с mTLS | Streaming для real-time событий, строгие контракты, аутентификация |
| Файловый доступ | SSHFS напрямую между клиентами (порт 2222) | Хаб не бутылочное горлышко, стандартный протокол |
| Аутентификация устройств | mTLS для gRPC + invite-код + SSH-ключи для SSHFS | Безопасно, хаб не видит приватных секретов |
| Обмен SSH-ключами | Публичные ключи через хаб при pairing | Публичные ключи не секрет; прямой канал для обмена ещё не установлен |
| Хранение на хабе | SQLite (pure Go, modernc.org/sqlite) | Файловая БД без CGO, не нужен отдельный сервис |
| Формат конфига | KDL | Предпочтение пользователя |
| Конфликты записи | Не обрабатываются | Hubfuse — доступ, не синхронизация |
| Центральный узел | Выделенный сервер | Простота на старте |
| Hot-reload конфига | fsnotify | Мгновенная реакция на изменения без перезапуска |
| Версионирование протокола | protocol_version в Register | Совместимость при обновлениях |
