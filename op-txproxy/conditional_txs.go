package op_txproxy

import (
	"context"
	"fmt"
	"time"

	"github.com/ethereum-optimism/optimism/op-service/client"
	"github.com/ethereum-optimism/optimism/op-service/metrics"
	"github.com/ethereum-optimism/optimism/op-service/predeploys"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"

	"github.com/prometheus/client_golang/prometheus"

	"golang.org/x/time/rate"
)

var (
	// errs
	rateLimitErr             = &JsonError{Message: "rate limited", Code: types.TransactionConditionalRejectedErrCode}
	endpointDisabledErr      = &JsonError{Message: "endpoint disabled", Code: types.TransactionConditionalRejectedErrCode}
	missingAuthenticationErr = &JsonError{Message: "missing authentication", Code: types.TransactionConditionalRejectedErrCode}
	entrypointSupportErr     = &JsonError{Message: "only 4337 Entrypoint contract support", Code: types.TransactionConditionalRejectedErrCode}
)

type ConditionalTxService struct {
	log log.Logger
	cfg *CLIConfig

	limiter             *rate.Limiter
	backend             client.RPC
	entrypointAddresses map[common.Address]bool

	costSummary prometheus.Summary
	requests    prometheus.Counter
	failures    *prometheus.CounterVec
}

func NewConditionalTxService(ctx context.Context, log log.Logger, m metrics.Factory, cfg *CLIConfig) (*ConditionalTxService, error) {
	rpc, err := client.NewRPC(ctx, log, cfg.SendRawTransactionConditionalBackend)
	if err != nil {
		return nil, fmt.Errorf("failed to dial backend %s: %w", cfg.SendRawTransactionConditionalBackend, err)
	}

	rpcMetrics := metrics.MakeRPCClientMetrics("backend", m)
	backend := client.NewInstrumentedRPC(rpc, &rpcMetrics)

	limiter := rate.NewLimiter(types.TransactionConditionalMaxCost, int(cfg.SendRawTransactionConditionalRateLimit))
	entrypointAddresses := map[common.Address]bool{predeploys.EntryPoint_v060Addr: true, predeploys.EntryPoint_v070Addr: true}

	return &ConditionalTxService{
		log: log,
		cfg: cfg,

		limiter:             limiter,
		backend:             backend,
		entrypointAddresses: entrypointAddresses,

		costSummary: m.NewSummary(prometheus.SummaryOpts{
			Namespace: MetricsNameSpace,
			Name:      "txconditional_cost",
			Help:      "summary of cost observed by *accepted* conditional txs",
		}),
		requests: m.NewCounter(prometheus.CounterOpts{
			Namespace: MetricsNameSpace,
			Name:      "txconditional_requests",
			Help:      "number of conditional transaction requests",
		}),
		failures: m.NewCounterVec(prometheus.CounterOpts{
			Namespace: MetricsNameSpace,
			Name:      "txconditional_failures",
			Help:      "number of conditional transaction failures",
		}, []string{"err"}),
	}, nil
}

func (s *ConditionalTxService) SendRawTransactionConditional(ctx context.Context, txBytes hexutil.Bytes, cond types.TransactionConditional) (common.Hash, error) {
	s.requests.Inc()
	if !s.cfg.SendRawTransactionConditionalEnabled {
		s.failures.WithLabelValues("disabled").Inc()
		return common.Hash{}, endpointDisabledErr
	}

	// Ensure the request is authenticated
	authInfo := AuthFromContext(ctx)
	if authInfo == nil {
		s.failures.WithLabelValues("missing auth").Inc()
		return common.Hash{}, missingAuthenticationErr
	}

	// Handle the request. For now, we do nothing with the authenticated signer
	hash, err := s.sendCondTx(ctx, authInfo.Caller, txBytes, &cond)
	if err != nil {
		s.failures.WithLabelValues(err.Error()).Inc()
		s.log.Error("failed transaction conditional", "caller", authInfo.Caller.String(), "hash", hash.String(), "err", err)
		return common.Hash{}, err
	}

	return hash, err
}

func (s *ConditionalTxService) sendCondTx(ctx context.Context, caller common.Address, txBytes hexutil.Bytes, cond *types.TransactionConditional) (common.Hash, error) {
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(txBytes); err != nil {
		return common.Hash{}, fmt.Errorf("failed to unmarshal tx: %w", err)
	}

	txHash, cost := tx.Hash(), cond.Cost()

	// external checks (tx target, conditional cost & validation)
	if tx.To() == nil || !s.entrypointAddresses[*tx.To()] {
		return txHash, entrypointSupportErr
	}
	if err := cond.Validate(); err != nil {
		return txHash, &JsonError{
			Message: fmt.Sprintf("failed conditional validation: %s", err),
			Code:    types.TransactionConditionalRejectedErrCode,
		}
	}
	if cost > types.TransactionConditionalMaxCost {
		return txHash, &JsonError{
			Message: fmt.Sprintf("conditional cost, %d, exceeded max: %d", cost, types.TransactionConditionalMaxCost),
			Code:    types.TransactionConditionalCostExceededMaxErrCode,
		}
	}

	// enforce rate limit on the cost to be observed
	ctxwt, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.limiter.WaitN(ctxwt, cost); err != nil {
		return txHash, rateLimitErr
	}

	s.costSummary.Observe(float64(cost))
	s.log.Info("broadcasting conditional transaction", "caller", caller.String(), "hash", txHash.String())
	return txHash, s.backend.CallContext(ctx, nil, "eth_sendRawTransactionConditional", txBytes, cond)
}
