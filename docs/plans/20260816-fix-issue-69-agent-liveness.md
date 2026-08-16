# Issue #69: ложная регистрация после prune и потеря liveness при реконсиляции маунтов

## Overview

Пир может «висеть живым» с точки зрения процесса, но быть отсутствующим/офлайн с точки зрения
хаба. В наблюдаемом сценарии удалённая шара монтируется сразу после re-join/pairing и исчезает
через 30 секунд, когда heartbeat-монитор хаба помечает пира офлайн.

Три независимых дефекта складываются в каскад (формулировки — из issue #69):

1. **Ложный успех прикладного ответа.** `HubClient.Register` и `HubClient.Heartbeat` смотрят
   только на транспортную ошибку gRPC и игнорируют `Success=false`
   (`internal/agent/client.go:91`, `:115`). Демон стартовавшей *удалённой* (pruned) идентичности
   печатает `registered with hub`, пишет PID-файл и продолжает работать, хотя хаб его не знает.
2. **Liveness зависит от монтирования.** `Daemon.Run` вызывает `registerAndSubscribe` до
   `runServices` (`internal/agent/daemon.go:434`), а `sessionOnce` внутри себя выполняет
   синхронную реконсиляцию маунтов (`processInitialDevices`, `:528`). Один устаревший маунт
   сжигает `mountVerifyTimeout` (10s, `internal/agent/mounter.go:375`) под локом маунтера —
   heartbeat-горутина стартует только после этого. Хаб успевает пометить устройство офлайн.
3. **Нет восстановления по heartbeat.** `Registry.Heartbeat` только двигает `last_heartbeat`
   (`internal/hub/registry.go:166`). Устройство, уже помеченное офлайн, остаётся офлайн навсегда,
   пока не переустановится сессия; пиры не получают `DeviceOnline` и не перемонтируют шару.
   Хуже: `store.UpdateHeartbeat` (`internal/hub/store/sqlite.go:203`) не смотрит на RowsAffected,
   поэтому heartbeat *удалённого* устройства тоже «успешен».

## Context (from discovery)

- `internal/agent/client.go` — обёртки RPC. `Success` проверяется только в `ConfirmPairing`
  (`:201`) и `Leave` (`:150`); `Register`/`Heartbeat`/`UpdateShares`/`Deregister` — нет.
  `Rename` возвращает respons наверх, `Success` проверяет CLI (`cmd/hubfuse/main.go:716`).
- `internal/agent/daemon.go` — `Run` (`:409`), `sessionOnce` (`:497`), `runServices` (`:638`),
  `processInitialDevices` (`:1004`). Сиды `registerFn`/`subscribeFn`/`updateSharesFn`
  (`:114-122`) — образец для инжектируемого heartbeat.
- `internal/agent/heartbeat.go` — `runHeartbeat`, тикер 10s захардкожен, дергает `d.hubClient`
  напрямую (в юнит-тестах `hubClient` = nil).
- `internal/hub/registry.go` — `Register` (`:91`), `Heartbeat` (`:166`), `MarkOffline` (`:359`).
  `MarkOffline` **не** закрывает подписку — стрим офлайн-устройства остаётся живым, поэтому
  восстановление по heartbeat действительно достижимо.
- `internal/hub/heartbeat.go` — `checkStale` берёт только `status='online'`; `pruneInactive`
  удаляет `status='offline'` старше retention и рассылает `DeviceRemoved`, закрывая подписку.
- `internal/hub/store/sqlite.go` — `GetDevice` (`:131`) заворачивает `sql.ErrNoRows` в
  `fmt.Errorf` (сентинела нет), `UpdateHeartbeat` (`:203`) игнорирует RowsAffected.
- `internal/hub/server.go` — `Register`/`Heartbeat` кодируют ошибки реестра в `Success=false`;
  `peerIP(ctx)` (`:263`) доступен и в `Heartbeat`.
- Хаб: таймаут heartbeat захардкожен (`NewHeartbeatMonitor(..., 0, ...)` в `internal/hub/hub.go`),
  хотя параметр у конструктора есть. Флага/конфига нет — сценарные тесты не могут его ускорить.
- Сценарный харнесс: `tests/scenarios/helpers/agent.go` (`StartDaemon`, `Mount` ждёт маркер,
  `WaitForDaemonLog`, `PeerStatus`), `tests/tools/stub-sshfs` (реальный SSH+SFTP хендшейк,
  затем JSON-маркер). ACL default-deny (`tests/scenarios/permissions_test.go:206`) заставляет
  `sftpClient.ReadDir` стаба упасть → маркер не появится → `Mount` сожжёт весь verify-таймаут.
  Это детерминированный рычаг для «устаревшего/недоступного маунта» без sleep-ов.

## Development Approach

- **testing approach**: Regular (код, затем тесты — в рамках каждой задачи)
- завершать каждую задачу полностью перед переходом к следующей
- маленькие сфокусированные изменения
- **CRITICAL: каждая задача обязана включать новые/обновлённые тесты** для затронутого кода
  - тесты не опциональны — это обязательная часть чеклиста
  - success + error сценарии
- **CRITICAL: все тесты зелёные перед началом следующей задачи** — без исключений
- **CRITICAL: обновлять этот план при изменении скоупа**
- обратная совместимость: пути #47/#49/#50/#61/#67 не должны деградировать; протокол
  (`proto/hubfuse.proto`) не меняется — старые агенты продолжают работать

## Testing Strategy

- **unit tests**: `internal/agent` (сиды `registerFn`/`heartbeatFn`, мокнутый маунтер),
  `internal/hub` (реестр + in-memory store), `internal/hub/store` (RowsAffected/сентинел),
  `cmd/hubfuse-hub` (резолв флага)
- **integration tests** (`tests/integration`, in-process хаб через `hubtest`): прикладные
  ответы RPC — Register удалённого устройства, Heartbeat неизвестного устройства,
  восстановление offline→online и рассылка `DeviceOnline`
- **scenario tests** (`tests/scenarios`, stub-sshfs): e2e-воспроизведение issue —
  (а) агент со «сломанным» исходящим маунтом остаётся онлайн ≥3 heartbeat-таймаутов, а его
  шара у пира остаётся смонтированной; (б) pruned-идентичность падает при старте с
  внятной ошибкой и не пишет `registered with hub`
- Полный прогон: `make build && make vet && make test`

## Progress Tracking

- отмечать выполненное `[x]` сразу по завершении
- новые обнаруженные задачи — с префиксом ➕
- блокеры — с префиксом ⚠️
- при отклонении от скоупа обновлять план

## Solution Overview

Выбранный подход — **исправить три дефекта на их собственных уровнях, не трогая протокол**:

1. **Клиент честно репортит прикладной отказ.** Обёртки `HubClient` возвращают Go-ошибку при
   `Success=false`. Сообщение приходит от хаба, поэтому agent не занимается разбором строк.
2. **Liveness стартует сразу после успешного Register, до любой работы с маунтами.**
   `startHeartbeatOnce` (sync.Once) вызывается из `sessionOnce` между Register и
   `processInitialDevices`; вызов в `runServices` остаётся идемпотентной страховкой.
3. **Хаб восстанавливает устройство по валидному heartbeat.** `Registry.Heartbeat` при
   `status=offline` переводит в `online` (свежий IP из `peerIP`, сохранённый ssh-порт и шары
   из стора) и рассылает `DeviceOnline` — пиры перемонтируют. Только `offline → online`;
   `registered → online` не трогаем (устройство ещё ни разу не регистрировалось, его ssh-порт
   в сторе не авторитетен).

**Отвергнутые альтернативы:**

- *Асинхронная реконсиляция маунтов в `processInitialDevices`* (вынести маунты в горутину /
  «кик» монитору). Это переписывает семантику `HUBFUSE_MOUNT_MONITOR_INTERVAL<=0`, ломает
  существующие юнит-тесты `TestProcessInitialDevices_AutoMounts*` и требует правил сброса
  backoff'а — ради устранения задержки `Subscribe`, которую issue не требует чинить.
  Задержка стрима остаётся известным осознанным non-goal: события, потерянные в этом окне,
  добирает монитор маунтов (#67).
- *Флаг `must_register` в `HeartbeatResponse`* (агент сам перерегистрируется). Требует
  изменения протокола и механики перезапуска сессии, не лечит уже выпущенные агенты
  (v0.1.1) и даёт худшее время восстановления, чем прямой флип на хабе.
- *Самоубийство демона при `Success=false` в heartbeat.* Отвергнуто: рестарт-петля не
  чинит pruned-идентичность (нужен `join`), а стартовый Register уже падает громко.

## Technical Details

### Store (`internal/hub/store`)

- Сентинел `ErrNotFound` в пакете store (слой БД не должен импортировать `internal/common` с
  gRPC-статусами). `GetDevice`/`GetDeviceByNickname` возвращают `%w`-обёрнутый `ErrNotFound`
  при `sql.ErrNoRows`; `UpdateHeartbeat` смотрит `RowsAffected() == 0` → `ErrNotFound`.
- Реестр транслирует `store.ErrNotFound` → `common.ErrDeviceNotFound` (gRPC NotFound), чтобы
  прикладной слой отвечал одинаково независимо от реализации стора.

### Hub (`internal/hub`)

- `Registry.Heartbeat(ctx, deviceID, ip)`:
  1. `UpdateHeartbeat` (ошибка «нет такого устройства» → `common.ErrDeviceNotFound`);
  2. `GetDevice`; если `Status != offline` — выход (никаких лишних событий);
  3. `UpdateDeviceStatus(online, ip, device.SSHPort)`, где `ip` — свежий адрес вызывающего
     (`peerIP`), с фолбэком на `device.LastIP`, если адрес не определился;
  4. `GetShares` → `Broadcast(DeviceOnline, excludeDevice=deviceID)`;
  5. INFO-лог `device recovered via heartbeat`.
- `Server.Heartbeat` передаёт `peerIP(ctx)` и возвращает `Success=false` + текст ошибки
  (поле `error` в `HeartbeatResponse` **не** добавляем — протокол не меняем; текст пойдёт в
  лог агента через generic-сообщение).
- `Server.Register` при `common.ErrDeviceNotFound` формирует actionable-текст:
  `device not found on hub — re-join with 'hubfuse join <hub-address> --token <token>'`.
  Остальные ошибки — как раньше (`err.Error()`).
- Гонка с параллельным `Register` того же устройства безвредна: оба пути идемпотентно ставят
  `online`; порядок событий у пира разрешает `Mount` (remount по смене эндпоинта, #61).
  Гонка с `checkStale` тоже безвредна: `MarkOffline` только снимает статус, а следующий
  heartbeat вернёт его.

### Agent (`internal/agent`)

- `HubClient`: `Register`, `Heartbeat`, `UpdateShares`, `Deregister`, `Rename` возвращают
  ошибку при `Success=false`. Формат: `register rejected by hub: <error>` (у ответов без поля
  `error` — `heartbeat rejected by hub`). CLI `rename` теряет свою ветку `!resp.Success` и
  просто оборачивает ошибку в `clierrors`.
- `Daemon.heartbeatFn` — сид над `HubClient.Heartbeat` (как `registerFn`), чтобы юнит-тесты
  наблюдали heartbeat без живого gRPC. `runHeartbeat` использует сид; при nil — Error-лог и
  выход (защита для `buildTestDaemon`).
- `Daemon.heartbeatInterval` — 10s по умолчанию, переопределяется
  `HUBFUSE_HEARTBEAT_INTERVAL` (тестовая ручка, как `HUBFUSE_MOUNT_MONITOR_INTERVAL`).
  Значение `<= 0` или мусор → WARN + дефолт: молча выключенный heartbeat = гарантированный
  офлайн, такого варианта у ручки нет. `runHeartbeat` дополнительно клампит интервал, чтобы
  `time.NewTicker(0)` не паниковал в тестовых демонах.
- `startHeartbeatOnce(ctx)` — `sync.Once`, запускает `go d.runHeartbeat(ctx)`. Вызывается из
  `sessionOnce` сразу после успешного `Register` (до `readyOnce`/`processInitialDevices`) и,
  как страховка, из `runServices`. ctx — время жизни демона (и `Run`, и `supervise` передают
  именно его), поэтому горутина живёт ровно одну; повторные сессии её не плодят.

### Тестовые ручки

- Хаб: флаг `--heartbeat-timeout` + ключ `heartbeat-timeout` в `config.kdl`
  (`resolveHeartbeatTimeout` по образцу `resolveDeviceRetention`), проброс в
  `hub.Config.HeartbeatTimeout` → `NewHeartbeatMonitor`. `0` = дефолт (30s), отрицательное —
  ошибка. Это же полезно операторам на медленных линках.
- Агент: `HUBFUSE_HEARTBEAT_INTERVAL` (см. выше).

### Сценарный рычаг «устаревший маунт»

Пир экспортирует шару с default-deny ACL (`WithExportACL(dir, "blocked", "ro")` без `--allow`).
Хаб её анонсирует, значит она попадает в `mountsForOnlineDevice`, но `stub-sshfs` падает на
`ReadDir` (`sftp.ErrSSHFxPermissionDenied`) и выходит до записи маркера — `Mount` честно
сжигает `mountVerifyTimeout`. Никаких sleep-ов и SIGSTOP.

## Tasks

### Task 1: store — сентинел ErrNotFound и честный UpdateHeartbeat

- [ ] `internal/hub/store/store.go`: экспортировать `ErrNotFound`, обновить doc-комментарии
      `GetDevice`/`GetDeviceByNickname`/`UpdateHeartbeat`
- [ ] `internal/hub/store/sqlite.go`: маппинг `sql.ErrNoRows` → `ErrNotFound`;
      `UpdateHeartbeat` проверяет `RowsAffected`
- [ ] тесты `internal/hub/store/sqlite_test.go`: `errors.Is(err, ErrNotFound)` для обоих
      геттеров и для heartbeat несуществующего устройства; успешный heartbeat по-прежнему nil

### Task 2: hub — восстановление устройства по heartbeat

- [ ] `Registry.Heartbeat(ctx, deviceID, ip)`: трансляция `store.ErrNotFound`, флип
      `offline → online`, broadcast `DeviceOnline`, INFO-лог
- [ ] `Server.Heartbeat`: передать `peerIP`, вернуть `Success=false` при ошибке
- [ ] `Server.Register`: actionable-текст для `common.ErrDeviceNotFound`
- [ ] тесты `internal/hub/registry_test.go`: (а) heartbeat онлайн-устройства не шлёт событий;
      (б) heartbeat офлайн-устройства переводит в online, шлёт `DeviceOnline` с шарами и
      свежим IP, не шлёт его самому устройству; (в) heartbeat неизвестного устройства —
      `common.ErrDeviceNotFound`

### Task 3: agent client — прикладные ошибки перестают быть «успехом»

- [ ] `internal/agent/client.go`: проверка `Success` в `Register`, `Heartbeat`,
      `UpdateShares`, `Deregister`, `Rename`
- [ ] `cmd/hubfuse/main.go`: убрать мёртвую ветку `!resp.Success` в `rename`
- [ ] тесты `tests/integration`: Register удалённого устройства → ошибка с подсказкой re-join;
      Heartbeat неизвестного устройства → ошибка; успешные пути не сломаны

### Task 4: agent — heartbeat стартует до реконсиляции маунтов

- [ ] `Daemon.heartbeatFn` + `Daemon.heartbeatInterval` + `mountMonitorIntervalFromEnv`-образный
      парсер `heartbeatIntervalFromEnv`
- [ ] `startHeartbeatOnce` и его вызов из `sessionOnce` (после успешного Register, до
      `processInitialDevices`) + страховочный вызов в `runServices`
- [ ] `runHeartbeat` использует сид и клампит интервал
- [ ] тесты `internal/agent/daemon_test.go`: (а) heartbeat случается, пока
      `processInitialDevices` ещё заблокирован в `Mount`; (б) heartbeat стартует ровно один раз
      на несколько сессий; (в) провалившийся Register не стартует heartbeat и не пишет PID;
      (г) парсер env (валид/мусор/ноль/отрицательное)

### Task 5: тестовые ручки таймингов

- [ ] хаб: `hub.Config.HeartbeatTimeout`, флаг `--heartbeat-timeout`, ключ конфига,
      `resolveHeartbeatTimeout`
- [ ] тесты `cmd/hubfuse-hub/config_resolve_test.go`: флаг побеждает конфиг, конфиг
      побеждает дефолт, отрицательное значение — ошибка
- [ ] `tests/scenarios/helpers`: `StartHubWithHeartbeatTimeout` (или опции у `StartHub`),
      `Agent.AddMount` (конфиг без ожидания маркера), `Agent.StartDaemonExpectFailure`

### Task 6: сценарные регресс-тесты issue #69

- [ ] `tests/scenarios/liveness_test.go`: агент с недоступным исходящим маунтом остаётся
      online ≥3 heartbeat-таймаутов, а его шара у пира не отваливается
- [ ] `tests/scenarios/prune_test.go` (или новый файл): pruned-идентичность стартует и падает
      с внятной ошибкой; в логе нет `registered with hub`

### Task 7: документация

- [ ] `CLAUDE.md`: обновить описания `internal/agent/daemon.go` (порядок liveness),
      `internal/hub` (восстановление по heartbeat, новый флаг), тестовых ручек
- [ ] перенести этот план в `docs/plans/completed/` **в этой же ветке до мержа**
