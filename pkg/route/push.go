package route

import (
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gedour/gedour/pkg/audit"
	"github.com/gedour/gedour/pkg/config"
	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/notify"
	"github.com/gedour/gedour/pkg/notify/webpush"
	"github.com/gedour/gedour/pkg/secret"
)

// MaxDevices caps one user's push devices, as MaxChannels caps channels.
const MaxDevices = 20

// pushParallel bounds one delivery's concurrent pushes: MaxDevices/pushParallel rounds of SendTimeout fit sendTimeout.
const pushParallel = 5

// pushTestTimeout cuts a "Send test" fan-out, or a rule test's channels in a row, short of the write timeout,
// so the answer still reaches the browser.
// A var for tests.
var pushTestTimeout = config.HTTPWriteTimeout - 5*time.Second

const webPushUnavailable = "Web Push is off: the stored VAPID key does not open with GEDOUR_ENCRYPTION_KEY"

// errNoDevice ends a web_push delivery with nothing to send to (010-AC9).
var errNoDevice = notify.Permanent(errors.New("no device"))

// errPartlySent marks a web_push delivery every device answered, some with a permanent refusal: it is sent,
// and last_error names the refusals.
var errPartlySent = errors.New("sent")

// partlySent returns the refusals of a delivery sent to its other devices.
func partlySent(err error) (string, bool) {
	if !errors.Is(err, errPartlySent) {
		return "", false
	}
	return strings.TrimPrefix(err.Error(), errPartlySent.Error()+"; "), true
}

// PushDevice is a device as the API returns it: never its endpoint or keys.
type PushDevice struct {
	ID             string     `json:"id" format:"uuid"`
	Name           string     `json:"name"`
	UserAgent      string     `json:"user_agent" doc:"The browser's user agent at registration"`
	EndpointSHA256 string     `json:"endpoint_sha256" doc:"Hex SHA-256 of the subscription endpoint; the endpoint itself is never returned"`
	CreatedAt      time.Time  `json:"created_at"`
	LastPushAt     *time.Time `json:"last_push_at" nullable:"true" doc:"When a push service last accepted a message for it"`
}

// deviceAudit is what the audit log records of a device: no endpoint, no keys.
type deviceAudit struct {
	PushDevice
	OwnerID string `json:"owner_id"`
}

// deviceOwner is all a move records: the device and who held it.
type deviceOwner struct {
	ID      string `json:"id"`
	OwnerID string `json:"owner_id,omitempty"`
}

type pushDeviceOutput struct {
	Body PushDevice
}

type listPushDevicesOutput struct {
	Body struct {
		Items []PushDevice `json:"items" doc:"Sorted by name"`
	}
}

type pushKeyOutput struct {
	Body struct {
		PublicKey string `json:"public_key" doc:"VAPID public key, base64url: the browser's applicationServerKey"`
	}
}

type registerPushDeviceInput struct {
	Body struct {
		Endpoint string `json:"endpoint" minLength:"1" maxLength:"2048" doc:"PushSubscription.endpoint; write-only"`
		Keys     struct {
			P256DH string `json:"p256dh" maxLength:"128" doc:"base64url P-256 public key; write-only"`
			Auth   string `json:"auth" maxLength:"64" doc:"base64url 16-byte auth secret; write-only"`
		} `json:"keys"`
		Name      string `json:"name" minLength:"1" maxLength:"100"`
		UserAgent string `json:"user_agent,omitempty" maxLength:"512"`
	}
}

type renamePushDeviceInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Name string `json:"name" minLength:"1" maxLength:"100"`
	}
}

// sealedSub is the sealed form of a subscription.
type sealedSub struct {
	Endpoint string `json:"endpoint"`
	P256DH   []byte `json:"p256dh"`
	Auth     []byte `json:"auth"`
}

const deviceColumns = `id::text, name, user_agent, encode(endpoint_hash, 'hex'), created_at, last_push_at, owner_id::text`

const deviceSelect = `SELECT ` + deviceColumns + ` FROM push_devices`

func scanDevice(r pgx.Row) (deviceAudit, error) {
	var d deviceAudit
	err := r.Scan(&d.ID, &d.Name, &d.UserAgent, &d.EndpointSHA256, &d.CreatedAt, &d.LastPushAt, &d.OwnerID)
	return d, err
}

func (s *Service) registerPush(api huma.API) {
	tags := []string{"push"}
	huma.Register(api, huma.Operation{
		OperationID: "get-push-key", Method: http.MethodGet, Path: "/api/v1/push/key",
		Summary: "The VAPID public key browsers subscribe with; 503 while Web Push is off", Tags: tags,
	}, s.pushKey)
	huma.Register(api, huma.Operation{
		OperationID: "list-push-devices", Method: http.MethodGet, Path: "/api/v1/push/devices",
		Summary: "The caller's push devices; endpoints and keys are never returned", Tags: tags,
	}, s.listDevices)
	huma.Register(api, huma.Operation{
		OperationID: "register-push-device", Method: http.MethodPost, Path: "/api/v1/push/devices",
		Summary: "Register a browser's push subscription; a known endpoint updates its device", Tags: tags, DefaultStatus: http.StatusCreated,
	}, s.registerDevice)
	huma.Register(api, huma.Operation{
		OperationID: "rename-push-device", Method: http.MethodPatch, Path: "/api/v1/push/devices/{id}",
		Summary: "Rename one of the caller's push devices", Tags: tags,
	}, s.renameDevice)
	huma.Register(api, huma.Operation{
		OperationID: "delete-push-device", Method: http.MethodDelete, Path: "/api/v1/push/devices/{id}",
		Summary: "Remove a push device; channels that listed it stop sending to it", Tags: tags, DefaultStatus: http.StatusNoContent,
	}, s.deleteDevice)
}

func (s *Service) pushKey(ctx context.Context, _ *struct{}) (*pushKeyOutput, error) {
	if _, err := s.me(ctx); err != nil {
		return nil, err
	}
	if s.sender.VAPID == nil {
		return nil, huma.Error503ServiceUnavailable(webPushUnavailable)
	}
	out := &pushKeyOutput{}
	out.Body.PublicKey = s.sender.VAPID.PublicKey()
	return out, nil
}

func (s *Service) listDevices(ctx context.Context, _ *struct{}) (*listPushDevicesOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, deviceSelect+` WHERE owner_id = $1 ORDER BY lower(name), id`, me)
	if err != nil {
		return nil, s.fail("list-push-devices", err)
	}
	out := &listPushDevicesOutput{}
	out.Body.Items, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (PushDevice, error) {
		d, err := scanDevice(r)
		return d.PushDevice, err
	})
	if err != nil {
		return nil, s.fail("list-push-devices", err)
	}
	if out.Body.Items == nil {
		out.Body.Items = []PushDevice{}
	}
	return out, nil
}

// decodeKey reads a base64url value, padded or not.
func decodeKey(v string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(v), "="))
}

// checkSubscription validates what the browser gave; the endpoint passes the webhook URL checks (010-AC1).
func (s *Service) checkSubscription(endpoint, p256dh, auth string) (sealedSub, []error) {
	sub := sealedSub{Endpoint: strings.TrimSpace(endpoint)}
	var errs []error
	if msg := s.checkTargetURL(sub.Endpoint, "https://push.example.com/send/abc"); msg != "" {
		errs = append(errs, &huma.ErrorDetail{Location: "body.endpoint", Message: msg})
	}
	var err error
	if sub.P256DH, err = decodeKey(p256dh); err != nil || len(sub.P256DH) != 65 {
		errs = append(errs, &huma.ErrorDetail{Location: "body.keys.p256dh", Message: "a base64url uncompressed P-256 point (65 bytes)"})
	} else if _, err := ecdh.P256().NewPublicKey(sub.P256DH); err != nil {
		errs = append(errs, &huma.ErrorDetail{Location: "body.keys.p256dh", Message: "not a point on P-256"})
	}
	if sub.Auth, err = decodeKey(auth); err != nil || len(sub.Auth) != 16 {
		errs = append(errs, &huma.ErrorDetail{Location: "body.keys.auth", Message: "a base64url 16-byte secret"})
	}
	return sub, errs
}

func openSub(box *secret.Box, id string, ct, nonce []byte) (webpush.Subscription, error) {
	plain, err := box.Open(ct, nonce, []byte(id))
	if err != nil {
		return webpush.Subscription{}, fmt.Errorf("device keys: %w", err)
	}
	var sub sealedSub
	if err := json.Unmarshal(plain, &sub); err != nil {
		return webpush.Subscription{}, fmt.Errorf("device keys: %w", err)
	}
	return webpush.Subscription(sub), nil
}

// registerDevice inserts the subscription, or updates the device that has its endpoint: the caller's, or
// another user's, which moves to the caller as the browser now belongs to whoever registered last (010-AC3).
func (s *Service) registerDevice(ctx context.Context, in *registerPushDeviceInput) (*pushDeviceOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	name := strings.TrimSpace(b.Name)
	sub, errs := s.checkSubscription(b.Endpoint, b.Keys.P256DH, b.Keys.Auth)
	if err := checkDeviceName(name); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, huma.Error400BadRequest("invalid device", errs...)
	}
	hash := sha256.Sum256([]byte(sub.Endpoint))
	ua := strings.ToValidUTF8(strings.ReplaceAll(b.UserAgent, "\x00", ""), "")
	plain, _ := json.Marshal(sub) // a string and two byte slices always encode
	out := &pushDeviceOutput{}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		// The user lock first, as in every user-scoped transaction: it serializes this user's registrations.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR NO KEY UPDATE`, me); err != nil {
			return err
		}
		before, err := scanDevice(tx.QueryRow(ctx, deviceSelect+` WHERE endpoint_hash = $1 FOR UPDATE`, hash[:]))
		known := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		id, action := uuid.NewString(), "push_device.registered"
		switch {
		case known && before.OwnerID == me:
			id, action = before.ID, "push_device.updated"
		case known:
			id, action = before.ID, "push_device.moved"
		}
		if action != "push_device.updated" {
			if err := underCap(ctx, tx, me, "push_devices", "push devices", MaxDevices); err != nil {
				return err
			}
		}
		ct, nonce := s.box.Seal(plain, []byte(id))
		if known {
			// A move starts the device afresh: the new owner sees nothing of its life with the old one.
			_, err = tx.Exec(ctx, `UPDATE push_devices SET owner_id = $2, name = $3, sealed = $4, nonce = $5, key_version = $6, user_agent = $7,
					created_at = CASE WHEN $8 THEN now() ELSE created_at END, last_push_at = CASE WHEN $8 THEN NULL ELSE last_push_at END
				WHERE id = $1`, id, me, name, ct, nonce, secret.KeyVersion, ua, action == "push_device.moved")
		} else {
			_, err = tx.Exec(ctx, `INSERT INTO push_devices (id, owner_id, name, endpoint_hash, sealed, nonce, key_version, user_agent)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, id, me, name, hash[:], ct, nonce, secret.KeyVersion, ua)
			if isUniqueViolation(err) {
				return huma.Error409Conflict("this browser was registered at the same time by another account: retry")
			}
		}
		if err != nil {
			return err
		}
		if action == "push_device.moved" {
			// The previous owner's channels must not reach the new owner's browser.
			if _, err := tx.Exec(ctx, `DELETE FROM channel_push_devices WHERE device_id = $1`, id); err != nil {
				return err
			}
		}
		after, err := scanDevice(tx.QueryRow(ctx, deviceSelect+` WHERE id = $1`, id))
		if err != nil {
			return err
		}
		out.Body = after.PushDevice
		e := audit.Entry{ActorUserID: me, Action: action, TargetType: "push_device", TargetID: id, After: after}
		switch action {
		case "push_device.updated":
			e.Before = before
		case "push_device.moved":
			// One entry per owner, each naming that owner only: neither learns who else holds the browser.
			// Background: the old owner's entry must not name an admin acting as the caller, nor the caller's API key.
			left := audit.Entry{Action: action, TargetType: "push_device", TargetID: id, RequestID: audit.RequestID(ctx),
				Before: deviceOwner{ID: id, OwnerID: before.OwnerID}, After: deviceOwner{ID: id}}
			if err := audit.Record(context.Background(), tx, left); err != nil {
				return err
			}
			e.Before, e.After = deviceOwner{ID: id}, deviceOwner{ID: id, OwnerID: me}
		}
		return audit.Record(ctx, tx, e)
	})
	if err != nil {
		return nil, s.fail("register-push-device", err)
	}
	return out, nil
}

// checkDeviceName refuses a blank name, and a NUL, which a text column refuses.
func checkDeviceName(name string) error {
	switch {
	case name == "":
		return &huma.ErrorDetail{Location: "body.name", Message: "must not be blank"}
	case strings.ContainsRune(name, 0):
		return &huma.ErrorDetail{Location: "body.name", Message: "no NUL character"}
	}
	return nil
}

// ownDevice locks one of owner's devices; anyone else's is a 404, like a missing one (010-AC2).
func ownDevice(ctx context.Context, tx pgx.Tx, owner, id string) (deviceAudit, error) {
	if uuid.Validate(id) != nil {
		return deviceAudit{}, huma.Error404NotFound("device not found")
	}
	d, err := scanDevice(tx.QueryRow(ctx, deviceSelect+` WHERE id = $1 AND owner_id = $2 FOR UPDATE`, id, owner))
	if errors.Is(err, pgx.ErrNoRows) {
		return d, huma.Error404NotFound("device not found")
	}
	return d, err
}

func (s *Service) renameDevice(ctx context.Context, in *renamePushDeviceInput) (*pushDeviceOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(in.Body.Name)
	if err := checkDeviceName(name); err != nil {
		return nil, huma.Error400BadRequest("invalid device", err)
	}
	out := &pushDeviceOutput{}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := ownDevice(ctx, tx, me, in.ID)
		if err != nil {
			return err
		}
		after := before
		after.Name = name
		if _, err := tx.Exec(ctx, `UPDATE push_devices SET name = $2 WHERE id = $1`, in.ID, name); err != nil {
			return err
		}
		out.Body = after.PushDevice
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "push_device.renamed", TargetType: "push_device", TargetID: in.ID, Before: before, After: after})
	})
	if err != nil {
		return nil, s.fail("rename-push-device", err)
	}
	return out, nil
}

func (s *Service) deleteDevice(ctx context.Context, in *idPath) (*struct{}, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := ownDevice(ctx, tx, me, in.ID)
		if err != nil {
			return err
		}
		// Cascades to channel_push_devices: every list naming it drops it (010-AC5).
		if _, err := tx.Exec(ctx, `DELETE FROM push_devices WHERE id = $1`, in.ID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "push_device.deleted", TargetType: "push_device", TargetID: in.ID, Before: before})
	})
	if err != nil {
		return nil, s.fail("delete-push-device", err)
	}
	return nil, nil
}

// validatePushConfig normalizes a web_push config: devices "all" or a non-empty list of device ids.
func validatePushConfig(raw map[string]any) (any, []error) {
	var c notify.WebPushConfig
	if err := decodeConfig(raw, &c); err != nil {
		return nil, []error{err}
	}
	switch v := c.Devices.(type) {
	case string:
		if v == notify.AllDevices {
			return c, nil
		}
	case []any:
		ids := []string{}
		var errs []error
		for i, x := range v {
			s, _ := x.(string)
			id, err := uuid.Parse(s)
			if err != nil {
				errs = append(errs, configError("devices["+strconv.Itoa(i)+"]", "a device id", x))
				continue
			}
			if !slices.Contains(ids, id.String()) {
				ids = append(ids, id.String())
			}
		}
		if len(v) == 0 {
			errs = append(errs, configError("devices", "pick at least one device, or all", nil))
		}
		c.Devices = ids
		return c, errs
	}
	return c, []error{configError("devices", `"all", or a list of your device ids`, c.Devices)}
}

// storedConfig is cfg as the channels row keeps it: a device list lives in channel_push_devices.
func storedConfig(typ string, cfg map[string]any) map[string]any {
	if typ != notify.TypeWebPush || cfg["devices"] == notify.AllDevices {
		return cfg
	}
	out := maps.Clone(cfg)
	delete(out, "devices")
	return out
}

// setPushDevices replaces the channel's device list; a device that is not the owner's is a 400 (010-AC5).
// SHARE: a concurrent delete or move of a listed device waits for this list, then drops the device from it.
func setPushDevices(ctx context.Context, tx pgx.Tx, owner, channel string, cfg map[string]any) error {
	list, _ := cfg["devices"].([]any)
	ids := make([]string, 0, len(list))
	for _, x := range list {
		s, _ := x.(string)
		ids = append(ids, s)
	}
	if len(ids) > 0 {
		rows, err := tx.Query(ctx, `SELECT id::text FROM push_devices WHERE owner_id = $1 AND id = ANY($2::uuid[]) FOR SHARE`, owner, ids)
		if err != nil {
			return err
		}
		mine, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		var errs []error
		for i, id := range ids {
			if !slices.Contains(mine, id) {
				errs = append(errs, configError("devices["+strconv.Itoa(i)+"]", "no such device among yours", id))
			}
		}
		if len(errs) > 0 {
			return huma.Error400BadRequest("invalid channel", errs...)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM channel_push_devices WHERE channel_id = $1`, channel); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO channel_push_devices (channel_id, device_id) SELECT $1, unnest($2::uuid[])`, channel, ids)
	return err
}

type pushTarget struct {
	id          string
	ct, nonce   []byte
	alreadySent bool
}

// pushSend fans d out to the channel's devices and returns how many accepted it now. A delivery (d.ID set)
// skips the devices that accepted it before and records the new ones; a test (d.ID 0) keeps no ledger. A
// gone device is removed and audited, never a failure (010-AC7, 010-AC8, 010-AC9).
func pushSend(ctx context.Context, pool *pgxpool.Pool, box *secret.Box, sender *notify.Sender, ch notify.Channel, d notify.Delivery) (int, error) {
	var ids []int64
	if d.ID != 0 {
		ids = []int64{d.ID}
	}
	return pushFanOut(ctx, pool, box, sender, ch, ids, func(silent bool) ([]byte, string) {
		return notify.PushPayload(d, silent), notify.PushUrgency(silent, d.Rule)
	})
}

// pushFanOut sends one message for the deliveries ids (none for a test). A device is done once it accepted
// every one of them: a digest records its acceptance against each, and a retry joined by a new delivery
// reaches every device again (012-AC12).
func pushFanOut(ctx context.Context, pool *pgxpool.Pool, box *secret.Box, sender *notify.Sender, ch notify.Channel, ids []int64,
	message func(silent bool) ([]byte, string)) (int, error) {
	var c notify.WebPushConfig
	if err := json.Unmarshal(ch.Config, &c); err != nil {
		return 0, fmt.Errorf("web push config: %w", err)
	}
	rows, err := pool.Query(ctx, `SELECT p.id::text, p.sealed, p.nonce,
			COALESCE(cardinality($3::bigint[]), 0) > 0 AND NOT EXISTS (SELECT 1 FROM unnest($3::bigint[]) x (id)
				WHERE NOT EXISTS (SELECT 1 FROM delivery_push_sent s WHERE s.delivery_id = x.id AND s.device_id = p.id))
		FROM push_devices p JOIN channels c ON c.owner_id = p.owner_id
		WHERE c.id = $1 AND ($2 OR p.id IN (SELECT device_id FROM channel_push_devices WHERE channel_id = $1))
		ORDER BY p.created_at, p.id`, ch.ID, c.Devices == notify.AllDevices, ids)
	if err != nil {
		return 0, fmt.Errorf("load devices: %w", err)
	}
	targets, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (pushTarget, error) {
		var t pushTarget
		err := r.Scan(&t.id, &t.ct, &t.nonce, &t.alreadySent)
		return t, err
	})
	if err != nil {
		return 0, fmt.Errorf("load devices: %w", err)
	}
	if len(targets) == 0 {
		return 0, errNoDevice
	}
	pending := slices.DeleteFunc(slices.Clone(targets), func(t pushTarget) bool { return t.alreadySent })
	payload, urgency := message(c.Silent)
	results := make([]error, len(pending))
	sem := make(chan struct{}, pushParallel)
	var wg sync.WaitGroup
	for i, t := range pending {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			sub, err := openSub(box, t.id, t.ct, t.nonce)
			if err != nil {
				results[i] = notify.Permanent(err)
				return
			}
			results[i] = sender.Push(ctx, sub, payload, urgency)
		})
	}
	wg.Wait()

	var accepted, retry, refused []string
	var gone []pushTarget
	for i, err := range results {
		switch {
		case err == nil:
			accepted = append(accepted, pending[i].id)
		case errors.Is(err, notify.ErrGone):
			gone = append(gone, pending[i])
		case errors.Is(err, notify.ErrPermanent):
			refused = append(refused, err.Error())
		default:
			retry = append(retry, err.Error())
		}
	}
	// Recorded even when the drain cut the sends: what was accepted must not go out again.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := recordPushes(rctx, pool, ids, accepted, gone); err != nil {
		return len(accepted), err
	}
	done := len(accepted) + len(targets) - len(pending)
	total := len(targets) - len(gone)
	failures := strings.Join(append(retry, refused...), "; ")
	switch {
	case len(retry) > 0:
		return len(accepted), fmt.Errorf("web push: %d of %d devices failed: %s", len(retry)+len(refused), total, failures)
	case done == 0 && len(refused) == 0:
		return 0, errNoDevice
	case done == 0:
		return 0, notify.Permanent(fmt.Errorf("web push: %s", failures))
	case len(refused) > 0:
		return len(accepted), fmt.Errorf("%w; %d of %d devices refused: %s", errPartlySent, len(refused), total, failures)
	}
	return len(accepted), nil
}

// recordPushes stamps the accepted devices, adds them to the deliveries' ledger, and removes the gone ones.
func recordPushes(ctx context.Context, pool *pgxpool.Pool, deliveryIDs []int64, accepted []string, gone []pushTarget) error {
	if len(accepted) == 0 && len(gone) == 0 {
		return nil
	}
	err := db.InTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE push_devices SET last_push_at = now() WHERE id = ANY($1::uuid[])`, accepted); err != nil {
			return err
		}
		if len(deliveryIDs) > 0 {
			if _, err := tx.Exec(ctx, `INSERT INTO delivery_push_sent (delivery_id, device_id)
				SELECT x, id FROM push_devices, unnest($1::bigint[]) x WHERE id = ANY($2::uuid[]) ON CONFLICT DO NOTHING`, deliveryIDs, accepted); err != nil {
				return err
			}
		}
		for _, g := range gone {
			// The nonce changes on every registration: a browser registered again since the push keeps its device.
			before, err := scanDevice(tx.QueryRow(ctx, `DELETE FROM push_devices WHERE id = $1 AND nonce = $2 RETURNING `+deviceColumns, g.id, g.nonce))
			if errors.Is(err, pgx.ErrNoRows) {
				continue // removed or registered again meanwhile
			}
			if err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, audit.Entry{Action: "push_device.gone", TargetType: "push_device", TargetID: g.id, Before: before}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("record pushes: %w", err)
	}
	return nil
}
