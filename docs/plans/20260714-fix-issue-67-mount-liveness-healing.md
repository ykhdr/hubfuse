# Issue #67: обнаружение и лечение мёртвых sshfs-маунтов (Transport endpoint is not connected)

## Overview

Маунт, чей sshfs-процесс умер (ENOTCONN-зомби), никогда не обнаруживается и не заменяется —
`ls` по таргету бесконечно возвращает `Transport endpoint is not connected`, даже когда пир
давно вернулся в сеть. #61 закрыл роуминг со сменой IP; этот план закрывает мёртвый маунт на
*неизменном* эндпоинте и без-событийные окна.

Три корневых пробела (из issue #67):
1. Same-endpoint ветка `Mount` (`internal/agent/mounter.go:307-311`) возвращает nil, не
   проверяя живость — `DeviceOnline` вернувшегося пира с тем же IP превращается в no-op.
2. Нет мониторинга живости: если события не пришло (обрыв < 30s heartbeat-таймаута, либо
   собственный event-stream наблюдателя был мёртв в момент `DeviceOffline`), ничто не
   триггерит никаких действий с маунтом.
3. `processInitialDevices` только добавляет/обновляет записи из Register-снапшота, но не
   удаляет пиров, ушедших офлайн, пока стрим был мёртв — `onlineDevices` навсегда врёт.

Дизайн-документ (подходы и обоснование выбора): см. Solution Overview ниже; полная версия —
scratchpad `design-issue-67.md` брейншторм-сессии.

## Context (from discovery)

- `internal/agent/mounter.go` — `Mount` (same-endpoint ветка :306, remount-ветка #61),
  `mountpointGoneCtx` (bounded-проба, семантика «таймаут ≠ мёртв»), `unmountKey`
  (force/reguard, reap #47, bounded #50), сиды `checkMountpoint`/`unmount`/`execCommand`.
- `internal/agent/daemon.go` — `runServices` (точка запуска фоновых горутин),
  `processInitialDevices`, `mountsForOnlineDevice`, `isPaired`, инжектируемый
  `minReconnectInterval` (образец для интервала монитора).
- `internal/agent/events.go` — `handleDeviceOnline`/`handleDeviceOffline` (образец heal-политики).
- `tests/tools/stub-sshfs/main.go` — пишет JSON-маркер с PID, блокируется на SIGTERM,
  удаляет маркер при выходе (defer). Не создаёт реальный FUSE-маунт.
- `tests/scenarios/helpers/` — `Agent.Mount` поллит маркер, `ReadMarker`, `WithEnv`,
  `sanitizeForMarker` (зеркало sanitize стаба).
- В стаб-режиме (`HUBFUSE_STUB_MOUNT_DIR`) `checkMountpoint` захардкожен `(true, nil)` —
  сценарные тесты не могут выразить «sshfs умер»; unmount-команды в стаб-режиме всегда
  падают (fusermount по не-маунтпоинту), но никто это не ассертит.
- Юнит-сиды: `SetMountpointCheckForTests`, `SetUnmountForTests`, фейки
  `registerFn`/`subscribeFn` в `daemon_test.go`.

## Development Approach

- **testing approach**: Regular (код, затем тесты — в рамках каждой задачи)
- завершать каждую задачу полностью перед переходом к следующей
- маленькие сфокусированные изменения
- **CRITICAL: каждая задача обязана включать новые/обновлённые тесты** для затронутого кода
  - тесты не опциональны — это обязательная часть чеклиста
  - success + error сценарии
- **CRITICAL: все тесты зелёные перед началом следующей задачи** — без исключений
- **CRITICAL: обновлять этот план при изменении скоупа**
- обратная совместимость: пути #47/#49/#50/#61 не должны деградировать

## Testing Strategy

- **unit tests**: обязательны в каждой задаче (`internal/agent`), через существующие сиды
- **scenario tests** (`tests/scenarios`, stub-sshfs): end-to-end воспроизведение сценария
  пользователя — убить stub-sshfs процесс = «sshfs умер», наблюдать самолечение
- **e2e/UI**: отсутствуют в проекте — N/A
- Полный прогон: `make build && make vet && make test` (unit + integration + cli + scenarios)

## Progress Tracking

- отмечать выполненное `[x]` сразу по завершении
- новые обнаруженные задачи — с префиксом ➕
- блокеры — с префиксом ⚠️
- при отклонении от скоупа обновлять план

## Solution Overview

Выбран подход **«проверка живости в Mount + периодический монитор в daemon»** (полный разбор
альтернатив — в дизайн-доке брейншторма):

- **Отвергнуто:** супервизия sshfs-процесса через foreground `-f` — меняет жизненный цикл
  процессов на обоих FUSE-стеках (macFUSE/FUSE-T), `reapMountCmd` полагается на демонизацию,
  не ловит wedged-but-alive состояния.
- **Отвергнуто как полное решение:** только фикс same-endpoint ветки — без-событийные окна
  остаются сломанными (ровно кейс пользователя «IP не менялся»). Оставлен как фаза 1.

Ключевые принципы:
- **Монитор действует только на подтверждённо-мёртвые маунты.** Таймаут пробы
  (повисший, но возможно живой FUSE) = «не подтверждено» = не трогаем. Живой маунт пира,
  которого хаб считает офлайн, — не трогаем.
- Переиспользуем `mountpointGoneCtx` (её семантика «таймаут → false» ровно та, что нужна)
  и teardown-ветку #61 (`unmountKey(force=true, reguard=false)` + fall-through в обычный
  mount-флоу) — новых механизмов размонтирования не появляется.
- Починка стаб-харнесса делает жизненный цикл стаба достоверным (маркер+PID = «жив»),
  что позволяет сценарно тестировать и этот фикс, и будущие.

## Technical Details

### Проба живости (Mounter)

- Константа `mountProbeTimeout = 3 * time.Second` (совпадает с recheck-окном `unmountKey`).
- Same-endpoint ветка `Mount`: `rctx := context.WithTimeout(ctx, mountProbeTimeout)`;
  `if !m.mountpointGoneCtx(rctx, existing.LocalPath) { return nil }` — иначе INFO-лог
  «re-mounting dead mount at same endpoint» и переход в существующую teardown+remount ветку
  (тот же код, что при смене эндпоинта; условие ветки обобщается).
- `DeadMounts(ctx) []*Mount`: снапшот `activeMounts` под `m.mu`, пробы — ВНЕ лока
  (повисший stat не должен блокировать все операции маунтера), каждая проба со своим
  `mountProbeTimeout`-под-ctx. Возвращает подтверждённо-мёртвые. Гонка «проба/параллельный
  unmount» безвредна: heal-путь заново проверяет состояние под `m.mu` внутри `Mount`.

### Монитор (Daemon)

- Поле `mountMonitorInterval time.Duration`; default 15s (между heartbeat 10s и таймаутом
  30s); `<= 0` — монитор не запускается. Env-переопределение
  `HUBFUSE_MOUNT_MONITOR_INTERVAL` (`time.ParseDuration`) читается в `NewDaemon` —
  тестовая ручка для сценариев (как `HUBFUSE_STUB_MOUNT_DIR`).
- `runServices`: `go d.runMountMonitor(ctx)`; тикер → `d.healDeadMounts(ctx)`.
- `healDeadMounts` — **лок-дисциплина как в `handleDeviceOnline`**: снапшот нужных данных
  (`onlineDevices` по nickname, указатель `cfg`) взять под `d.mu` и ОТПУСТИТЬ лок ДО любых
  вызовов маунтера — `Mount`/`UnmountDevice` берут `m.mu` и могут блокироваться до
  `mountVerifyTimeout` (10s) в verify-полле; держать `d.mu` поперёк — стойло для всех
  event-хендлеров. Маунтер никогда не берёт `d.mu`, дедлока нет — только латентность:
  1. `dead := d.mounter.DeadMounts(ctx)`
  2. под `d.mu` (RLock) снять снапшот: `cfg` + карта nickname → `*OnlineDevice`; отпустить.
  3. для каждого мёртвого `mnt`: найти пира по nickname == `mnt.Device`; найти `MountConfig`
     в `cfg.Mounts` по (Device, Share); проверить, что пир всё ещё экспортирует share и
     `isPaired`.
  4. пир онлайн + конфиг есть → `d.mounter.Mount(...)` — dead-ветка из фазы 1 делает
     teardown+remount атомарно под `m.mu`.
  5. пир офлайн → `d.mounter.UnmountDevice(nickname)` (зеркало `handleDeviceOffline`:
     force + reguard; следующий `DeviceOnline` смонтирует начисто).
  6. маунт удалён из конфига → `d.mounter.Unmount(device, share)` (интерактивный путь;
     recheck-reap добьёт зомби). Непарный — скип + WARN.
  7. ошибки Mount — лог, ретрай на следующем тике (без отдельного backoff в v1).
- Принятый шум: для wedged-but-alive маунта `mountpointGoneCtx` пишет WARN
  «mountpoint check did not complete» на каждом тике. Осознанно принято в v1 (сигнал
  админу, что маунт завис); при жалобах — тихий вариант пробы.

### Снапшот onlineDevices (Daemon)

- `processInitialDevices`: собрать set device_id из снапшота; под `d.mu` удалить из
  `onlineDevices` записи, которых нет в set. Ничего не размонтировать здесь — мёртвые
  маунты исчезнувших пиров подберёт монитор (консервативно: живой маунт не трогается).

### Стаб-харнесс (agent + tests)

- Новый файл `internal/agent/stubmount.go`:
  - `stubMountpointCheck(markerDir string) func(string) (bool, error)`: путь маркера =
    `filepath.Join(markerDir, sanitize(dst)+".json")` (sanitize зеркалит стаб и
    `helpers.sanitizeForMarker` — ТРЕТЬЯ копия в трёх пакетах, зафиксировать
    синхронизацию комментарием во всех трёх местах);
    маркера нет → `(false, nil)`; маркер есть, PID из JSON мёртв (`syscall.Kill(pid, 0)` →
    ESRCH) → `(false, nil)`; PID жив → `(true, nil)`; ошибка чтения/парсинга → `(false, nil)`.
    **Авторитетный сигнал смерти — отсутствие маркера** (defer стаба снимает его на
    SIGTERM); PID-проверка — вторичная защита и НЕ ловит незареапленного зомби
    (kill(pid,0) по зомби не даёт ESRCH) — SIGKILL-нутый стаб с уцелевшим маркером
    прочитается как живой; сценарии обязаны убивать стаб SIGTERM'ом.
  - `stubUnmount(markerDir string) func(ctx, path string, force bool) error`: прочитать
    маркер → SIGTERM PID → подождать исчезновения маркера (до ~2s поллинга) → nil;
    маркера нет → nil (уже мёртв); не исчез → error.
- `NewMounter` в стаб-режиме ставит обе функции вместо хардкода `(true, nil)` и `unmountPath`.
- Следствия: verify-полл `Mount` теперь реально ждёт маркер (в пределах 10s таймаута);
  упавший стаб = честная ошибка маунта; `handleDeviceOffline`/`UnmountDevice` в сценариях
  реально убивают стаб и снимают запись (заодно чинится латентный баг «unmount в
  стаб-режиме всегда падает и запись остаётся»). Затронуты только сценарии, создающие
  маунты: `mount_test.go` и `pair_confirm_test.go` — остальные (`permissions_`, `leave_`,
  `events_`, `reconnect_`) маунтов не создают; тем не менее прогнать весь пакет.

## What Goes Where

- **Implementation Steps** (`[ ]`): код, тесты, документация в этом репозитории
- **Post-Completion** (без чекбоксов): ручная проверка на живой паре машин пользователя

## Implementation Steps

### Task 1: Проба живости в same-endpoint ветке `Mount`

**Files:**
- Modify: `internal/agent/mounter.go`
- Modify: `internal/agent/mounter_test.go`

- [x] добавить `mountProbeTimeout = 3 * time.Second` рядом с `unmountOpTimeout`
- [x] в same-endpoint ветке `Mount` заменить безусловный `return nil` на bounded-пробу
      `mountpointGoneCtx`; «не подтверждено мёртв» (жив или таймаут) → `return nil`;
      «подтверждено мёртв» → INFO-лог и переход в существующую teardown+remount ветку
      (`unmountKey(force=true, reguard=false)` + fall-through)
- [x] обновить комментарий ветки (#61 → #61+#67 семантика)
- [x] тесты: живой маунт → no-op (unmount не вызывался, entry сохранён)
      (`TestMount_SameEndpointAliveProbeIsNoOp` + существующий
      `TestMount_SameEndpointIsSilentNoOp` не деградировал)
- [x] тесты: мёртвый — `(false, nil)` и `(false, ENOTCONN-err)` → teardown + новый mount
      на том же эндпоинте (unmount вызван, exec вызван, entry обновлён)
      (`TestMount_SameEndpointDeadRemounts`, таблица)
- [x] тесты: проба висит дольше таймаута → no-op (никакого teardown возможно-живого маунта)
      (`TestMount_SameEndpointHangingProbeIsNoOp`)
- [x] тесты: мёртвый + unmount падает + recheck говорит «всё ещё маунтпоинт» → ошибка,
      entry сохранён (`TestMount_SameEndpointDeadUnmountFailureAborts`)
- [x] `go test ./internal/agent/...` — зелёные перед задачей 2 (+ `go build ./...`,
      `go vet ./...`)

### Task 2: `Mounter.DeadMounts` — bounded-обход активных маунтов

**Files:**
- Modify: `internal/agent/mounter.go`
- Modify: `internal/agent/mounter_test.go`

- [x] метод `DeadMounts(ctx context.Context) []*Mount`: снапшот под `m.mu`, пробы вне лока,
      per-mount `mountProbeTimeout`, вернуть подтверждённо-мёртвые
- [x] докомментарий: почему пробы вне лока и почему гонка с параллельным unmount безвредна
- [x] тесты (таблица классификации): `(true,nil)` → жив; `(false,nil)` → мёртв;
      `(false,err)` → мёртв; проба висит → жив («не подтверждено»)
      (`TestDeadMounts_Classification`; плюс ➕ `TestDeadMounts_MixedReturnsOnlyConfirmedDead`
      — мульти-маунт свип возвращает только мёртвые, записи сохранены, и
      ➕ `TestDeadMounts_ProbesOutsideLock` — висящая проба не блокирует `IsActive`)
- [x] тест: пустой `activeMounts` → nil/пусто, ctx уважается
      (`TestDeadMounts_EmptyAndCancelledCtx` — отменённый ctx прерывает свип до проб)
- [x] `go test ./internal/agent/...` — зелёные перед задачей 3 (+ `go build ./...`,
      `go vet ./...`, `-race` прогон)

### Task 3: Монитор здоровья маунтов в daemon

**Files:**
- Modify: `internal/agent/daemon.go`
- Modify: `internal/agent/daemon_test.go`

- [x] поле `mountMonitorInterval time.Duration`; в `NewDaemon` default 15s + env-override
      `HUBFUSE_MOUNT_MONITOR_INTERVAL` (некорректное значение → WARN + default)
- [x] `runMountMonitor(ctx)`: тикер с `mountMonitorInterval`, на тик — `healDeadMounts(ctx)`;
      выход по `ctx.Done()`; не запускается при `<= 0`
- [x] `healDeadMounts(ctx)`: политика из Technical Details (онлайн → Mount; офлайн →
      UnmountDevice; нет в конфиге → Unmount; непарный → скип+WARN; ошибки → лог)
      (➕ также «пир онлайн, но share больше не экспортирует» → скип+WARN — консервативно,
      unmount-cleanup этого кейса принадлежит SharesUpdated/config-путям)
- [x] старт `go d.runMountMonitor(ctx)` в `runServices`
- [x] `healDeadMounts`: снапшот `onlineDevices`/`cfg` под `d.mu`, отпустить лок ДО вызовов
      маунтера (зеркало `handleDeviceOnline`; см. Technical Details; `OnlineDevice`
      копируются по значению вместе со slice `Shares` — `handleSharesUpdated` мутирует
      `info.Shares` in-place под `d.mu`)
- [x] тесты (у `Daemon.mounter` нет интерфейса — ассертить через сиды реального `Mounter`:
      `execCommand`-capture, `SetMountpointCheckForTests`, `SetUnmountForTests`, затем
      состояние `IsActive`/`ActiveMounts`; `buildTestDaemon` обходит `NewDaemon`, поэтому
      `mountMonitorInterval` выставлять на тест-демоне напрямую):
      мёртвый маунт + пир онлайн (экспортирует share, парный) → перемонтирован: в
      captured-args нового exec актуальные IP/port пира, entry обновлён
      (`TestHealDeadMounts_PeerOnlineRemounts` — таблица: same-endpoint dead-ветка и
      roamed-endpoint remount-ветка)
- [x] тесты: мёртвый маунт + пир офлайн → запись снята (unmount-сид вызван), живые маунты
      не тронуты (`TestHealDeadMounts_PeerOfflineUnmountsDeadOnly`)
- [x] тесты: маунт удалён из конфига → запись снята; непарный пир → ничего не вызвано
      (`TestHealDeadMounts_RemovedFromConfigUnmounts`,
      `TestHealDeadMounts_SkipsUnpairedAndUnexportedPeers` — таблица: непарный и
      не-экспортируемый share)
- [x] тесты: `runMountMonitor` — срабатывает по тику, останавливается по ctx, не стартует
      при интервале 0; env-override `HUBFUSE_MOUNT_MONITOR_INTERVAL` тестировать отдельным
      юнитом функции парсинга (путь через `NewDaemon` юнитам недоступен)
      (`TestRunMountMonitor_TicksAndStopsOnCancel`,
      `TestRunMountMonitor_DisabledIntervalReturnsImmediately`,
      `TestMountMonitorIntervalFromEnv`; ➕ wiring через `NewDaemon` оказался юнитам
      доступен — `TestNewDaemon_MountMonitorIntervalDefaultAndEnvOverride` покрывает
      default и env-override end-to-end)
- [x] `go test ./internal/agent/...` — зелёные перед задачей 4 (+ `go build ./...`,
      `go vet ./...`, `-race` прогон)

### Task 4: Пересборка `onlineDevices` из Register-снапшота

**Files:**
- Modify: `internal/agent/daemon.go`
- Modify: `internal/agent/daemon_test.go`

- [x] `processInitialDevices`: удалить под `d.mu` записи `onlineDevices`, отсутствующие в
      снапшоте (снапшот авторитетен); ничего не размонтировать в этом пути
- [x] докомментарий: почему unmount исчезнувших пиров отдан монитору (консервативность)
- [x] тесты: пир, ушедший за время дохлого стрима, удалён из map после ре-регистрации;
      присутствующие обновлены; их маунты в этом пути не тронуты
      (`TestProcessInitialDevices_PrunesPeersAbsentFromSnapshot` — прунинг + refresh IP
      выжившего пира + mount жив + unmount-сид ни разу не вызван)
- [x] `go test ./internal/agent/...` — зелёные перед задачей 5 (+ `go build ./...`,
      `go vet ./...`, `-race` прогон)

### Task 5: Достоверный стаб-харнесс (маркер+PID вместо хардкода)

**Files:**
- Create: `internal/agent/stubmount.go`
- Create: `internal/agent/stubmount_test.go`
- Modify: `internal/agent/mounter.go` (`NewMounter` — подключение стаб-функций)
- Modify: `tests/scenarios/*` (аудит/починка тестов, полагавшихся на старое поведение)

- [x] `stubMountpointCheck(markerDir)`: маркер отсутствует/нечитаем/PID мёртв → `(false, nil)`;
      PID жив → `(true, nil)`; sanitize зеркалит стаб (sync-комментарий добавлен во все
      ТРИ копии: стаб, helpers, stubmount.go)
- [x] `stubUnmount(markerDir)`: SIGTERM PID из маркера → ждать исчезновения маркера (≤2s);
      нет маркера → nil; не исчез → error (➕ уточнения: битый маркер без PID → быстрый
      error; ESRCH при живом маркере → error «killed without SIGTERM» — зомби-каветка;
      ожидание уважает ctx вызывающего)
- [x] `NewMounter`: в стаб-режиме ставить обе функции (вместо `(true,nil)`-хардкода и
      `unmountPath`)
- [x] юнит-тесты stubmount: жив/убит/нет маркера/битый JSON; stubUnmount успех и таймаут
      (`TestStubMountpointCheck_Classification`, `TestStubPIDAlive_ZombieCaveat`,
      `TestStubUnmount_*` ×5, `TestNewMounter_StubModeInstallsMarkerHarness`,
      `TestNewMounter_NoStubEnvKeepsRealHarness`, `TestStubSanitizePath`)
- [x] прогнать `make test-scenarios`; аудит только маунтящих сценариев (`mount_test.go`,
      `pair_confirm_test.go` — остальные маунтов не создают) — маунт теперь честно ждёт
      маркер (все сценарии зелёные без правок: оба маунтящих теста дожидаются пар/маркеров
      и удовлетворяют достоверной семантике; verify-полл демона проходит по живому маркеру)
- [x] `go test ./internal/agent/... && make test-scenarios` — зелёные перед задачей 6
      (+ `go build ./...`, `go vet ./...`, `-race` прогон)

### Task 6: Сценарные тесты самолечения (репро пользователя)

**Files:**
- Create: `tests/scenarios/heal_test.go`
- Modify: `tests/scenarios/helpers/agent.go` (хелпер KillStubMount — SIGTERM по PID из
  маркера; хелпер RestartDaemon — повторный старт БЕЗ повторного `share add`, шары уже
  в config.kdl)

- [x] хелпер `KillStubMount`: убить stub-sshfs процесс маунта по PID из маркера
      (строго SIGTERM — см. зомби-каветку Task 5), дождаться исчезновения маркера —
      симуляция «sshfs умер»
- [x] хелпер `RestartDaemon`: как `StartDaemon`, но без повторного добавления экспортов
      (сегодня повторный `StartDaemon` дублирует `share add`; ни один сценарий пока не
      делает Stop→Start одного агента) — общее ядро вынесено в `launchDaemon`
- [x] `TestMonitorRemountsDeadMount`: монитор 1s (env); alice экспортирует, bob маунтит;
      убить stub bob'а; без каких-либо offline/online циклов ждать нового маркера с новым
      PID (Eventually ≤15s) — закрывает «IP не менялся, событий нет»; это ЖЕ прогоняет
      dead-ветку `Mount` end-to-end (heal идёт через `healDeadMounts` → `Mount` →
      same-endpoint dead-branch)
- [x] `TestOfflineOnlineCycleRemountsMount`: монитор выключен (env 10m); bob маунтит;
      graceful-рестарт alice (`Stop` → `RestartDaemon`, тот же ssh-порт) → у bob
      `DeviceOffline` реально снимает маунт (truthful stubUnmount убивает стаб), затем
      `DeviceOnline` монтирует начисто (новый маркер, новый PID). Проверяет цепочку
      офлайн-reap → чистый remount, ранее нетестируемую в стаб-режиме.
      ПРИМЕЧАНИЕ ревью: событийный путь через dead-ветку `Mount` сценарно недостижим при
      живом хабе (graceful restart всегда шлёт DeviceOffline первым → entry снята до
      DeviceOnline); dead-ветка покрыта юнитами Task 1 и сценарием монитора выше
- ➕ хелпер `WaitForDaemonLog` (agent.go): обнаруженная гонка — `Mount()` возвращается по
      появлению маркера, до одного verify poll-interval (200ms) РАНЬШЕ, чем демон запишет
      маунт в `activeMounts`; убийство стаба в этом окне абортирует in-flight mount
      (записи нет — монитору нечего лечить). Оба heal-сценария ждут строки «mounted share»
      в логе демона перед kill/Stop
- ➕ хелпер `TryReadMarker` (marker.go): нефатальный reader маркера для
      `require.Eventually`-поллеров (stub пишет маркер неатомарно — поллер обязан
      переживать полузаписанный файл, `ReadMarker` фаталит)
- [x] `make test-scenarios` — зелёные перед задачей 7 (50.9s, оба новых сценария ×3
      без флейков; `go build ./...`, `go vet ./...`, `go test ./internal/agent/...`
      также зелёные)

### Task 7: Проверка критериев приёмки

- [x] мёртвый маунт + `DeviceOnline` с тем же эндпоинтом → перемонтирован (событийный путь)
      — dead-ветка `Mount` (`mounter.go` same-endpoint probe + teardown):
      `TestMount_SameEndpointDeadRemounts` (юнит) + сценарий
      `TestOfflineOnlineCycleRemountsMount` (offline-reap → чистый remount по событию)
- [x] мёртвый маунт + никаких событий → перемонтирован монитором в пределах интервала
      — `runMountMonitor`/`healDeadMounts`: `TestHealDeadMounts_PeerOnlineRemounts`,
      `TestRunMountMonitor_TicksAndStopsOnCancel` (юниты) + сценарий
      `TestMonitorRemountsDeadMount` (kill stub → новый PID без событий, монитор 1s)
- [x] мёртвый маунт + пир офлайн → снят и заguarжен; следующий `DeviceOnline` монтирует
      начисто — `healDeadMounts` → `UnmountDevice` (`unmountKey(force=true, reguard=true)`,
      `mounter.go:792`): `TestHealDeadMounts_PeerOfflineUnmountsDeadOnly` (юнит) + сценарий
      `TestOfflineOnlineCycleRemountsMount` (новый маркер/PID после DeviceOnline)
- [x] висящая проба → маунт не тронут (ни в `Mount`, ни в мониторе)
      — `TestMount_SameEndpointHangingProbeIsNoOp` (Mount) +
      `TestDeadMounts_Classification` (висящая проба → «не подтверждено» → жив) +
      `TestDeadMounts_ProbesOutsideLock` (висящая проба не блокирует маунтер)
- [x] `onlineDevices` после ре-регистрации соответствует снапшоту
      — `processInitialDevices` прунит отсутствующих (`daemon.go:919-933`):
      `TestProcessInitialDevices_PrunesPeersAbsentFromSnapshot`
- [x] пути #47/#49/#50/#61 не деградировали (существующие тесты зелёные:
      `internal/agent` полностью зелёный, включая -race -count=1; #50 bounded/force,
      #49 guard, #61 remount/supervise-сьюты в `mounter_test.go`/`daemon_test.go`)
- [x] полный прогон: `make build && make vet && make test` — exit 0
      (+ контрольные `go test ./tests/scenarios/... -count=1` 51s и
      `go test ./internal/agent/... -race -count=1` без кэша)

### Task 8: Документация и финализация

- [ ] CLAUDE.md: дополнить описания `daemon.go`/`mounter.go` в Agent internals (монитор
      здоровья, dead-ветка Mount, достоверный стаб) со ссылкой (issue #67)
- [ ] обновить план: все чекбоксы, ➕/⚠️ по факту
- [ ] переместить план в `docs/plans/completed/`

## Post-Completion

**Ручная проверка на живой паре (mac + ykhdrPC):**
- обновить оба демона, повторить сценарий из issue #67 (увести мак из сети >1 мин, вернуть
  с тем же IP) — маунт на сервере должен ожить сам в пределах ~30s
- проверить лог демона: `re-mounting dead mount at same endpoint` / heal-события монитора

**Осознанные компромиссы (не реализуем в v1):**
- backoff для повторно падающего remount (ретрай раз в тик; пересмотреть при жалобах на шум)
- обнаружение wedged-but-alive FUSE (таймаут пробы = «жив» by design — не рвём возможно
  живой маунт)
- никаких изменений hub/proto
