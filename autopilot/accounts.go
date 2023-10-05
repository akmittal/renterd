package autopilot

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	rhpv3 "go.sia.tech/core/rhp/v3"
	"go.sia.tech/core/types"
	"go.sia.tech/renterd/alerts"
	"go.sia.tech/renterd/api"
	"go.sia.tech/renterd/tracing"
	"go.uber.org/zap"
	"lukechampine.com/frand"
)

var errMaxDriftExceeded = errors.New("drift on account is too large")

var (
	alertAccountRefillID = frand.Entropy256() // constant across restarts

	minBalance  = types.Siacoins(1).Div64(2).Big()
	maxBalance  = types.Siacoins(1)
	maxNegDrift = new(big.Int).Neg(types.Siacoins(10).Big())
)

type accounts struct {
	ap *Autopilot
	a  AccountStore
	c  ContractStore
	l  *zap.SugaredLogger
	w  *workerPool

	refillInterval time.Duration

	mu                sync.Mutex
	inProgressRefills map[types.Hash256]struct{}
}

type AccountStore interface {
	Account(ctx context.Context, id rhpv3.Account, host types.PublicKey) (account api.Account, err error)
	Accounts(ctx context.Context) (accounts []api.Account, err error)
}

type ContractStore interface {
	Contracts(ctx context.Context) ([]api.ContractMetadata, error)
	ContractSetContracts(ctx context.Context, set string) ([]api.ContractMetadata, error)
}

func newAccounts(ap *Autopilot, a AccountStore, c ContractStore, w *workerPool, l *zap.SugaredLogger, refillInterval time.Duration) *accounts {
	return &accounts{
		ap: ap,
		a:  a,
		c:  c,
		l:  l.Named("accounts"),
		w:  w,

		refillInterval:    refillInterval,
		inProgressRefills: make(map[types.Hash256]struct{}),
	}
}

func (a *accounts) markRefillInProgress(workerID string, host types.PublicKey) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := types.HashBytes(append([]byte(workerID), host[:]...))
	_, inProgress := a.inProgressRefills[k]
	if inProgress {
		return false
	}
	a.inProgressRefills[k] = struct{}{}
	return true
}

func (a *accounts) markRefillDone(workerID string, host types.PublicKey) {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := types.HashBytes(append([]byte(workerID), host[:]...))
	_, inProgress := a.inProgressRefills[k]
	if !inProgress {
		panic("releasing a refill that hasn't been in progress")
	}
	delete(a.inProgressRefills, k)
}

func (a *accounts) refillWorkersAccountsLoop(stopChan <-chan struct{}) {
	ticker := time.NewTicker(a.refillInterval)

	for {
		select {
		case <-stopChan:
			return // shutdown
		case <-ticker.C:
		}

		a.w.withWorker(func(w Worker) {
			a.refillWorkerAccounts(w)
		})
	}
}

// refillWorkerAccounts refills all accounts on a worker that require a refill.
// To avoid slow hosts preventing refills for fast hosts, a separate goroutine
// is used for every host. If a slow host's account is still being refilled by a
// goroutine from a previous call, refillWorkerAccounts will skip that account
// until the previously launched goroutine returns.
func (a *accounts) refillWorkerAccounts(w Worker) {
	ctx, span := tracing.Tracer.Start(context.Background(), "refillWorkerAccounts")
	defer span.End()

	// fetch config
	state := a.ap.State()

	// fetch worker id
	workerID, err := w.ID(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to fetch worker id")
		a.l.Errorw(fmt.Sprintf("failed to fetch worker id for refill: %v", err))
		return
	}

	// fetch all contracts
	contracts, err := a.c.Contracts(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to fetch contracts")
		a.l.Errorw(fmt.Sprintf("failed to fetch contracts for refill: %v", err))
		return
	}

	// fetch all contract set contracts
	contractSetContracts, err := a.c.ContractSetContracts(ctx, state.cfg.Contracts.Set)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, fmt.Sprintf("failed to fetch contracts for set '%s'", state.cfg.Contracts.Set))
		a.l.Errorw(fmt.Sprintf("failed to fetch contract set contracts: %v", err))
		return
	}

	// build a map of contract set contracts
	inContractSet := make(map[types.FileContractID]struct{})
	for _, contract := range contractSetContracts {
		inContractSet[contract.ID] = struct{}{}
	}

	// refill accounts in separate goroutines
	for _, c := range contracts {
		// add logging for contracts in the set
		_, inSet := inContractSet[c.ID]

		// launch refill if not already in progress
		if a.markRefillInProgress(workerID, c.HostKey) {
			go func(contract api.ContractMetadata, inSet bool) {
				rCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
				accountID, refilled, rerr := refillWorkerAccount(rCtx, a.a, a.ap.bus, w, workerID, contract)
				shouldLog := rerr != nil && (inSet || rerr.Is(errMaxDriftExceeded))
				if shouldLog {
					a.l.Errorw(rerr.err.Error(), rerr.keysAndValues...)
				} else if err == nil && refilled {
					a.l.Infow("Successfully funded account",
						"account", accountID,
						"host", contract.HostKey,
						"balance", maxBalance,
					)
				}

				// handle registering alert.
				alertID := types.HashBytes(append(alertAccountRefillID[:], accountID[:]...))
				if shouldLog {
					data := map[string]interface{}{
						"accountID":  accountID.String(),
						"contractID": contract.ID.String(),
						"hostKey":    contract.HostKey.String(),
					}
					for i := 0; i < len(rerr.keysAndValues); i += 2 {
						data[fmt.Sprint(rerr.keysAndValues[i])] = rerr.keysAndValues[i+1]
					}
					err := a.ap.alerts.RegisterAlert(ctx, alerts.Alert{
						ID:        alertID,
						Severity:  alerts.SeverityError,
						Message:   fmt.Sprintf("failed to refill account: %v", err),
						Data:      data,
						Timestamp: time.Now(),
					})
					if rerr != nil {
						a.ap.logger.Errorf("failed to register alert: %v", err)
					}
				} else if err := a.ap.alerts.DismissAlerts(ctx, alertID); err != nil {
					a.ap.logger.Errorf("failed to dismiss alert: %v", err)
				}
				a.markRefillDone(workerID, contract.HostKey)
				cancel()
			}(c, inSet)
		}
	}
}

type refillError struct {
	err           error
	keysAndValues []interface{}
}

func (err *refillError) Is(target error) bool {
	return errors.Is(err.err, target)
}

func refillWorkerAccount(ctx context.Context, a AccountStore, am alerts.Alerter, w Worker, workerID string, contract api.ContractMetadata) (accountID rhpv3.Account, refilled bool, rerr *refillError) {
	wrapErr := func(err error, keysAndValues ...interface{}) *refillError {
		if err == nil {
			return nil
		}
		return &refillError{
			err:           err,
			keysAndValues: keysAndValues,
		}
	}

	// add tracing
	ctx, span := tracing.Tracer.Start(ctx, "refillAccount")
	span.SetAttributes(attribute.Stringer("host", contract.HostKey))
	defer func() {
		if rerr != nil {
			span.RecordError(rerr.err)
			span.SetStatus(codes.Error, "failed to refill account")
		}
		span.End()
	}()

	// fetch the account
	accountID, err := w.Account(ctx, contract.HostKey)
	if err != nil {
		rerr = wrapErr(err)
		return
	}
	var account api.Account
	account, err = a.Account(ctx, accountID, contract.HostKey)
	if err != nil {
		rerr = wrapErr(err)
		return
	}

	// update span
	span.SetAttributes(attribute.Stringer("account", account.ID))
	span.SetAttributes(attribute.Stringer("balance", account.Balance))

	// check if a host is potentially cheating before refilling.
	// We only check against the max drift if the account's drift is
	// negative because we don't care if we have more money than
	// expected.
	if account.Drift.Cmp(maxNegDrift) < 0 {
		rerr = wrapErr(fmt.Errorf("not refilling account since host is potentially cheating: %w", errMaxDriftExceeded),
			"account", account.ID,
			"host", contract.HostKey,
			"balance", account.Balance,
			"drift", account.Drift,
		)
		return
	}

	// check if a resync is needed
	if account.RequiresSync {
		// sync the account
		err = w.RHPSync(ctx, contract.ID, contract.HostKey, contract.HostIP, contract.SiamuxAddr)
		if err != nil {
			rerr = wrapErr(fmt.Errorf("failed to sync account's balance: %w", err),
				"account", account.ID,
				"host", contract.HostKey,
			)
			return
		}

		// refetch the account after syncing
		account, err = a.Account(ctx, accountID, contract.HostKey)
		if err != nil {
			rerr = wrapErr(err)
			return
		}
	}

	// check if refill is needed
	if account.Balance.Cmp(minBalance) >= 0 {
		rerr = wrapErr(err)
		return
	}

	// fund the account
	err = w.RHPFund(ctx, contract.ID, contract.HostKey, contract.HostIP, contract.SiamuxAddr, maxBalance)
	if err != nil {
		rerr = wrapErr(fmt.Errorf("failed to fund account: %w", err),
			"account", account.ID,
			"host", contract.HostKey,
			"balance", account.Balance,
			"expected", maxBalance,
		)
	} else {
		refilled = true
	}
	return
}
