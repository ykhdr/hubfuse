# Issue #61: переподключение агента и распространение нового IP при роуминге

## Overview

Агент вызывает `Register`/`Subscribe` ровно один раз — при старте. После любого сетевого блипа горутина чтения event-стрима навсегда завершается, и демон больше никогда не перерегистрируется. Из-за этого устройство, которое ушло из сети и вернулось (особенно с новым DHCP-адресом), не переанонсируется пирам: авто-маунт не восстанавливается — пир либо остаётся без mount, либо держит «протухший» mount на мёртвый старый IP. Восстановление возможно только ручным рестартом демона.

Чиним тремя изменениями **на стороне агента** (хаб не трогаем):

1. **Супервизор** — цикл, который при смерти стрима заново прогоняет `Register → Subscribe`.
2. **Перемонтирование при смене IP/порта** — `Mounter.Mount` сносит старый mount и монтирует заново, если endpoint пира изменился.
3. **sshfs reconnect-опции** — sshfs сам переустанавливает SSH-сессию при разрыве TCP без смены IP (same-IP блип).

**Почему хаб не меняем:** `Register` уже идемпотентен, обновляет сохранённый IP и ре-броадкастит `DeviceOnline` со свежим IP (`internal/hub/registry.go:115-149`). `Subscribe` уже закрывает-и-заменяет старый subscriber-канал при реконнекте (`internal/hub/registry.go:256-260`). Достаточно, чтобы агент заново зарегистрировался — хаб сделает всё остальное сам.

## Context (from discovery)

Файлы/компоненты:
- `internal/agent/daemon.go` — `registerAndSubscribe` (L393) и `Run` (L327): источник бага — одноразовый register + горутина чтения стрима, которая `return`-ит на первой ошибке `Recv()` (L414-428). `onReady()` вызывается на L403-405.
- `internal/agent/mounter.go` — `Mount` (L278) сразу возвращает `"already mounted"` при существующем ключе (L284-285). Структура `Mount` хранит `IP`/`SSHPort` (L187-188). `buildMountArgs` (L66) собирает аргументы. **`unmountKey(ctx, key, force, reguard)` уже вызывается под захваченным `m.mu`** (L619, L672) — готовый примитив для атомарного remount. `unmountLadder` содержит `fusermount -uz` (lazy) вторым шагом (L142-143). `unmountOpTimeout` используется в `unmountAll`.
- `internal/agent/connector.go` — константы backoff `backoffInitial`/`backoffMax` (1s→60s, L13-14) для переиспользования в супервизоре.
- `internal/agent/events.go` — `handleDeviceOnline` (L70) и др. вызывают `Mount`; чинятся автоматически через Компонент 2.
- `internal/agent/heartbeat.go` — heartbeat использует тот же `d.hubClient`; при reuse соединения синхронизация не нужна.
- `tests/integration/` — поднимают in-process хаб с in-memory SQLite, гоняют gRPC-флоу через **raw `pb.HubFuseClient`** (`integration_test.go:16-65`). Здесь НЕТ `agent.Daemon`/`Mounter`/супервизора — harness не подходит для проверки реконнекта/remount. (Файл `tests/integration/reconnect_test.go` уже существует — это тест cert-reuse при рестарте хаба, не про нашу фичу.)
- `tests/scenarios/` — запускают реальные подпроцессы демона `hubfuse` со `stub-sshfs` на PATH; маунты стабятся через `HUBFUSE_STUB_MOUNT_DIR` (`helpers/agent.go:229`), stub-маркер пишет `RemoteHost`/`RemotePort` (`tests/tools/stub-sshfs/main.go:134-135`). **Единственный harness, где реально крутится супервизор.** Уже есть `reconnect_test.go:TestAgentReconnectsAfterHubRestart`, `helpers/hub.go:151-174 Hub.Restart` (тот же порт → разрыв стрима + транспорт-реконнект), `helpers/status.go:30 PeerStatus`.

Паттерны:
- В `Mounter` уже всё инъектируется для тестов: `execCommand`, `unmount`, `checkMountpoint` (+ сеттеры `SetExecCommandForTests`, `SetUnmountForTests`). Юнит-тесты mounter детерминированны без реального FUSE.
- Версионирование/CLI/store — по `CLAUDE.md`, в этой задаче не затрагиваются.

Зависимости: gRPC `ClientConn` сам переустанавливает транспорт после блипа — `Register`/`Subscribe` на старом `hubClient` снова проходят, как только сеть вернулась.

## Development Approach

- **Подход к тестам: Regular** (сначала реализация, затем тесты — внутри каждой задачи, до перехода к следующей).
- Каждую задачу доводим до конца перед переходом к следующей; изменения мелкие, сфокусированные.
- **Каждая задача ОБЯЗАНА включать новые/обновлённые тесты** на изменённый код (успех + ошибки/краевые случаи).
- **Все тесты должны проходить перед началом следующей задачи** — без исключений.
- **Обновлять этот файл плана при изменении scope.**
- Прогонять тесты после каждого изменения; держать обратную совместимость.

## Testing Strategy

- **Юнит-тесты** (`internal/agent`): обязательны для каждой задачи с кодом. Детерминированны через инъекцию `execCommand`/`unmount`/`checkMountpoint`.
- **Сценарные тесты** (`tests/scenarios`): реальные подпроцессы демона + `stub-sshfs` — единственный harness, где крутится супервизор. Проверяют сквозной флоу reconnect-after-restart. (Remount под stub не воспроизводим — нет реального FUSE-mount; покрыт юнит-тестом mounter в Task 2.)
- **`tests/integration`** (raw `pb.HubFuseClient`): супервизора/mounter там нет — для этой задачи не используется.
- **E2E (UI):** в проекте нет.
- Существующие тесты, ассертящие ошибку `"already mounted"` для того же endpoint, обновить — теперь там `nil`.

## Progress Tracking

- Отмечать выполненное `[x]` сразу.
- Новые задачи помечать `➕`, блокеры — `⚠️`.
- Держать план в синхроне с реальной работой.

## Solution Overview

Высокоуровнево:

- **Супервизор.** `registerAndSubscribe` разбивается на `sessionOnce` (register + `onReady` через `sync.Once` + `processInitialDevices` + subscribe → возвращает стрим), `readStream` (текущее тело горутины), `reconnectSession` (цикл `sessionOnce` с backoff 1s→60s) и `supervise` (читает стрим, при смерти — реконнект). Первый `sessionOnce` синхронный в `Run` (сохраняет «hub down на старте → ошибка старта» и тайминг `onReady` для PID-файла). Дальше всё крутится в одной горутине `supervise` — маунты остаются сериализованными.
- **Перемонтирование при смене IP.** Внутри `Mount` под `m.mu`: тот же `IP`+`SSHPort` → тихий `nil` (раньше — ошибка); другой endpoint → `unmountKey` старого под тем же локом, затем обычный flow монтирования; нет mount → обычный flow. Покрывает гонку `tryMount` (горутина config-watcher) × `handleDeviceOnline` (горутина supervise) одним локом и чинит все 4 места вызова `Mount` сразу.
- **sshfs reconnect.** `buildMountArgs` добавляет `-o reconnect -o ServerAliveInterval=15 -o ServerAliveCountMax=3` в общие args — same-IP блип чинится самим sshfs в фоне.

Ключевые решения и обоснование:
- **Reuse `hubClient`** вместо редайла: меньше движущихся частей, heartbeat продолжает работать с тем же указателем без синхронизации, gRPC сам чинит транспорт.
- **Снапшот важнее эвентов:** порядок `Register → processInitialDevices → Subscribe` сохранён; полное состояние онлайн-устройств берётся из `RegisterResponse`, а не собирается из эвентов. `processInitialDevices` повторно прогоняется на каждом реконнекте — так роуминг-устройство обновляет свои собственные mount'ы.
- **Логика remount в `Mounter`, не в `Daemon`:** атомарность check-and-remount под `m.mu`, переиспользование готового `unmountKey`.

## Technical Details

**Супервизор (`daemon.go`):**
- **Тестовый seam:** `HubClient` — конкретная структура без интерфейса (`client.go:18`), застабить без живого gRPC нельзя. Вводим инъектируемые поля-функции на `Daemon` (в стиле `Mounter.execCommand`/`unmount`): `registerFn`/`subscribeFn`, по умолчанию делегируют `d.hubClient.Register`/`.Subscribe`; в тестах подменяются фейком. Даёт юнит-покрытие `sessionOnce`/`reconnectSession` без живого хаба.
- Новое поле `Daemon.readyOnce sync.Once`; onReady вызывается с **nil-guard**: `d.readyOnce.Do(func() { if d.onReady != nil { d.onReady() } })`. Прямой `Do(d.onReady)` паникует при nil `OnReady` (опционален — `daemon.go:30-36`; `buildTestDaemon` оставляет nil).
- `reconnectSession` слушает `ctx.Done()` в backoff-`select` и возвращает `nil` при отмене → `supervise` выходит.
- Частичный успех `sessionOnce` (register прошёл, subscribe упал) → повторяется весь `sessionOnce`; повторный `Register` безвреден (идемпотентен), лишний `DeviceOnline` для пиров = no-op (same-IP).
- `seedNicknamesFromHub` остаётся одноразовым в `Run` (кэш никнеймов уже тёплый) — в `sessionOnce` не переносим.

**Перемонтирование (`mounter.go`, в `Mount` после `m.mu.Lock()`):**
- `existing.IP == deviceIP && existing.SSHPort == sshPort` → `return nil`.
- Иначе: `rctx, cancel := context.WithTimeout(ctx, unmountOpTimeout)`; `m.unmountKey(rctx, key, true /*force*/, false /*reguard*/)`; `cancel()`. `force=true` — старый endpoint при смене IP, скорее всего, мёртв (force-ladder доходит до `umount -l`). `reguard=false` — сразу монтируем заново, а обычный flow ниже сам вызывает `guardTarget`. При ошибке unmount — вернуть ошибку, новый mount не начинать.
- Лог `Info` «re-mounting peer at new endpoint» с old/new IP.

**sshfs reconnect (`mounter.go`, `buildMountArgs`):**
- Добавить в базовый `args` (до `extraOpts`): `"-o", "reconnect"`, `"-o", "ServerAliveInterval=15"`, `"-o", "ServerAliveCountMax=3"`. Без нового конфиг-ключа, значения — константы.

## What Goes Where

- **Implementation Steps** (`[ ]`): код, тесты, обновление документации в этом репозитории.
- **Post-Completion** (без чекбоксов): ручная проверка роуминга на реальных macOS+Linux, верификация поддержки `reconnect` в macFUSE-sshfs / fuse-t-sshfs.

## Implementation Steps

### Task 1: sshfs reconnect-опции в buildMountArgs

**Files:**
- Modify: `internal/agent/mounter.go`
- Modify: `internal/agent/mounter_test.go`

- [x] в `buildMountArgs` (mounter.go:66) добавить в базовый `args` (до цикла `extraOpts`): `-o reconnect`, `-o ServerAliveInterval=15`, `-o ServerAliveCountMax=3`
- [x] значения вынести в именованные константы рядом с функцией (например `sshKeepaliveInterval`, `sshKeepaliveCountMax`)
- [x] обновить/добавить table-тест на `buildMountArgs`: аргументы содержат `reconnect` и keepalive-опции для бэкендов `sshfs` и `fuse-t`
- [x] убедиться, что относительный порядок операндов (`hubfuse@ip:share`, `to`) и `extraOpts` (`cache=no` для fuse-t) не нарушен
- [x] прогнать `go test ./internal/agent/... -run Mount` — должно пройти перед Task 2
- [x] коммит + пуш (по workflow `CLAUDE.md`) — push пропущен (нет GitHub-кредов в окружении); сделан только локальный коммит

### Task 2: Перемонтирование при смене IP/порта в Mounter.Mount

**Files:**
- Modify: `internal/agent/mounter.go`
- Modify: `internal/agent/mounter_test.go`

- [x] в `Mount` (mounter.go:294) заменить безусловную ошибку `"already mounted"` на ветвление под уже захваченным `m.mu`: same `IP`+`SSHPort` → `return nil`; другой endpoint → `unmountKey(ctxWithTimeout, key, true, false)` затем обычный flow; нет mount → обычный flow
- [x] обернуть remount-unmount в `context.WithTimeout(ctx, unmountOpTimeout)` с `cancel()`; при ошибке unmount вернуть оборачивающую ошибку (`"...: unmount stale endpoint ...: %w"`) и не монтировать новое
- [x] добавить `Info`-лог при перемонтировании («re-mounting peer at new endpoint» — device, share, old_ip/old_port → new_ip/new_port)
- [x] тест: existing **same** endpoint → `Mount` возвращает `nil`, повторный `execCommand` не вызывается, `unmount` не вызывается (`TestMount_SameEndpointIsSilentNoOp`)
- [x] тест: existing **другой** endpoint → старый `unmount` вызван (force=true), новый mount стартован с новым IP/портом (`TestMount_DifferentEndpointRemounts`, table: ip/port/both); плюс edge-case `TestMount_RemountUnmountFailureAborts` (unmount fail → ошибка, новый mount не стартует, stale entry удержан)
- [x] тест: **нет** mount → обычный flow (регресс сохранён) (`TestMount_NoExistingMountProceedsNormally`)
- [x] обновить существующий same-endpoint тест: ассерт ошибки `"already mounted"` → `NoError`; `TestMount_RejectsDuplicateMount` → `TestMount_SameEndpointIsSilentNoOp`, `TestGuardTarget_RemountActiveKeyDoesNotChmod` → `TestGuardTarget_SameEndpointRemountIsSilentNoOp` (намерение «rejected» → «silent no-op»); поведение «без `guardTarget`» сохранено (ранний `return nil` до `guardTarget`)
- [x] прогнать `go test ./internal/agent/...` — прошло (зелёный); `go build ./...` + `go vet ./...` чисто
- [x] коммит + пуш (по workflow `CLAUDE.md`) — push пропущен (нет GitHub-кредов в окружении); сделан только локальный коммит

### Task 3: Супервизор переподключения в daemon.go

**Files:**
- Modify: `internal/agent/daemon.go`
- Modify: `internal/agent/daemon_test.go`

- [x] добавить тестовый seam: поля-функции `registerFn`/`subscribeFn` на `Daemon` (в стиле `Mounter.execCommand`), по умолчанию делегируют `d.hubClient.Register`/`.Subscribe` (т.к. `HubClient` — конкретная структура без интерфейса, `client.go:18`) — дефолты ставятся лениво в `ensureSessionFns` (closures читают `d.hubClient` в момент вызова, т.к. при конструировании он ещё nil)
- [x] добавить поле `readyOnce sync.Once`; вызывать onReady с nil-guard: `d.readyOnce.Do(func() { if d.onReady != nil { d.onReady() } })`
- [x] выделить `sessionOnce(ctx) (<stream>, error)`: `registerFn` → onReady (через `readyOnce`) → `processInitialDevices` → `subscribeFn`, возвращает стрим
- [x] выделить `readStream(ctx, stream)`: текущее тело горутины (L414-428) — читает до `Recv`-ошибки или `ctx.Done()`
- [x] добавить `reconnectSession(ctx) <stream>`: цикл `sessionOnce` с backoff `backoffInitial`→`backoffMax`; слушает `ctx.Done()` в `select`, возвращает `nil` при отмене; `Info`-лог при успехе
- [x] добавить `supervise(ctx, stream)`: `for { readStream; if ctx.Err()!=nil return; warn "hub session lost"; stream = reconnectSession; if stream==nil return }`
- [x] переписать `registerAndSubscribe`: синхронный первый `sessionOnce` (ошибка прерывает старт), затем `go supervise(ctx, stream)`; сохранить порядок и тайминг onReady
- [x] тест: `readStream` с fake `pb.HubFuse_SubscribeClient`, возвращающим ошибку → выходит, не зацикливается (лёгкий, без seam) — `TestReadStream_ExitsOnRecvError`
- [x] тест: onReady вызывается ровно один раз при нескольких `sessionOnce` (через seam; nil-guard не паникует при nil onReady) — `TestSessionOnce_OnReadyFiresExactlyOnce` + `TestSessionOnce_NilOnReadyDoesNotPanic`
- [x] тест: `reconnectSession` выходит с `nil` при отменённом `ctx` (через seam с фейком, возвращающим ошибку) — `TestReconnectSession_ReturnsNilOnCancelledCtx`
- [x] прогнать `go test ./internal/agent/...` — прошло (зелёный, в т.ч. `-race`); `go build ./...` + `go vet ./...` чисто
- [x] коммит + пуш (по workflow `CLAUDE.md`) — push пропущен (нет GitHub-кредов в окружении); сделан только локальный коммит

### Task 4: Сценарные тесты восстановления (`tests/scenarios`)

**Files:**
- Modify: `tests/scenarios/reconnect_test.go`
- (при необходимости) Modify: `tests/scenarios/helpers/` — только если не хватает готовых хелперов

- [x] усилить `TestAgentReconnectsAfterHubRestart`: после `Hub.Restart` (`helpers/hub.go:151-174`, тот же порт → разрыв стрима + транспорт-реконнект) ассертить `PeerStatus(...) == "online"` (`helpers/status.go:30`), а не просто «строка присутствует» — финальный `require.Eventually` теперь требует `row.Status == "online"`; окно поллинга расширено до 20s (backoff 1+2+4+8s + старт хаба + RPC)
- [x] обновить устаревший комментарий теста (`reconnect_test.go:18-21`), который гласит, что ре-регистрация «не реализована» — переписан: супервизор (Task 3) детектит мёртвый Subscribe-стрим и заново прогоняет Register→Subscribe, хаб ре-маркит «online»
- [x] НЕ создавать `tests/integration/reconnect_test.go` — файл уже существует и не про это; integration-harness (raw `pb.HubFuseClient`) супервизор не гоняет — новый файл не создавался
- [x] **remount E2E сознательно НЕ делаем** под scenarios: stub не создаёт реальный FUSE-mount → remount-unmount (реальный `fusermount`/`umount` против обычной директории) не отцепит stub-маркер (`mountpointGoneCtx` через stub'нутый `checkMountpoint` считает mount «не исчез» → entry сохраняется → `RemotePort` не меняется), а рестарт экспортёра на новом ssh-порту harness сегодня не умеет. Endpoint-change remount детерминированно покрыт юнит-тестом mounter (Task 2); зафиксировано в Out of Scope — ничего не реализовано, решение оставлено как есть
- [x] прогнать `go test ./tests/scenarios/... -timeout 120s` — должно пройти перед Task 5 — зелёный; усиленный тест прогнан `-count=5` без флапа (~9s/прогон), полный suite 33s; `go build`/`go vet ./tests/scenarios/...` чисто
- [x] коммит + пуш (по workflow `CLAUDE.md`) — push пропущен (нет GitHub-кредов в окружении); сделан только локальный коммит

### Task 5: Проверка критериев приёмки

- [x] проверить, что все пункты Overview реализованы (супервизор реконнектится, новый IP распространяется, пиры перемонтируют, same-IP блип чинится sshfs) — сверено по коду: `daemon.go` `sessionOnce`/`readStream`/`reconnectSession`/`supervise` (L450-547) + `registerAndSubscribe` (sync 1-я сессия → `go supervise`); `processInitialDevices` повторно прогоняется на каждом реконнекте из снапшота `RegisterResponse` (хаб не тронут); `mounter.go` remount-ветка в `Mount` (L306-338); reconnect/keepalive-опции в `buildMountArgs` (L85-87)
- [x] проверить краевые случаи: `onReady` один раз; чистый выход супервизора по `ctx`; remount только при смене endpoint — `readyOnce.Do`+nil-guard (`daemon.go` L463-467); `reconnectSession` возвращает `nil` по `ctx.Done` (L519)→`supervise` выходит; same endpoint → ранний `return nil` (L307-312). Покрыто тестами: `TestSessionOnce_OnReadyFiresExactlyOnce`/`NilOnReadyDoesNotPanic`, `TestReconnectSession_ReturnsNilOnCancelledCtx`, `TestMount_SameEndpointIsSilentNoOp`/`DifferentEndpointRemounts`/`RemountUnmountFailureAborts`
- [x] `make build` — зелёный (чисто)
- [x] `make vet` — зелёный (чисто)
- [x] `make test` (unit + integration) и `go test ./tests/scenarios/... -timeout 120s` — всё зелёное: `internal/...` (agent 5.6s), `tests/integration` (9.3s), `tests/cli` (1.9s), `tests/scenarios` (33s); отдельный прогон scenarios с `-timeout 120s` — ok. Правок кода не потребовалось (всё прошло с первого раза)

### Task 6: Документация и финализация

**Files:**
- Modify: `CLAUDE.md` (если выявлены новые паттерны)

- [ ] обновить `CLAUDE.md`, если появился паттерн, достойный фиксации (например, «agent supervises hub session»)
- [ ] README обновлять не требуется (внешнего поведения CLI не меняем)
- [ ] перенести этот план в `docs/plans/completed/`
- [ ] финальный коммит + пуш; открыть PR в `master` (покоммитные изменения по задачам уже запушены в ветку `fix/issue-61-agent-reconnect`)

## Post-Completion

*Требуют ручного действия / внешних систем — без чекбоксов, информационно.*

**Ручная проверка:**
- Реальный роуминг-сценарий на двух машинах (macOS-клиент + Linux-сервер-хаб): Wi-Fi off → reconnect с новым DHCP-IP → сервер перемонтирует share на новый адрес без рестарта демона.
- Сценарий «reconnect в пределах 30s» (без offline-цикла): mount должен переехать на новый IP, а не остаться на мёртвом.
- **Remount vs non-empty-refusal guard (только реальный FUSE):** при remount после *lazy* unmount (`fusermount -uz`/`umount -l`) обычный flow заново вызывает `targetHasLocalContents` (`mounter.go:299-310`); медленный lazy-teardown может на миг показать старое удалённое содержимое и споткнуться о «refusing to mount over non-empty dir». Стаб/юнит этого не ловят (`targetHasLocalContents` под stub возвращает 0). Проверить на реальной машине.

**Верификация внешних зависимостей:**
- Подтвердить поддержку `-o reconnect` в установленных сборках sshfs: macFUSE-sshfs и fuse-t-sshfs (опция старая, но проверить, что не ломает монтирование на целевых платформах).
- Поведение открытых до разрыва файловых дескрипторов после sshfs-reconnect (ожидаемо EIO на старых FD, новые операции работают) — приемлемо для роуминга, зафиксировать в issue если всплывут нюансы.

## Out of Scope (сознательно)

- **Same-IP «протухший» mount, если sshfs reconnect не справился** (хвостовой случай). Liveness-пробу (`stat` точки монтирования с таймаутом) НЕ делаем: `stat` зависшего FUSE блокируется в D-state (uninterruptible, `ctx` не прерывает), плюс риск ложных remount живого, но медленного линка.
- **Потеря эвента в микроокне** между возвратом `Register` и установкой нового `Subscribe`-канала на хабе. Компенсируется снапшотом `RegisterResponse` (полное состояние онлайн-устройств берётся оттуда, не из эвентов). Окно субсекундное, текущее поведение не ухудшается.
- **Remount-E2E под `tests/scenarios`** — не делаем: stub не создаёт реальный FUSE-mount, поэтому remount-teardown не отцепит stub-маркер, а рестарт экспортёра на новом ssh-порту harness не поддерживает. Endpoint-change remount покрыт юнит-тестом mounter (Task 2); полноценный subprocess-remount потребовал бы менять хрупкую bounded-teardown логику (#50/#47) ради тестового ассерта.
