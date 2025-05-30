package faultproof_withdrawals

import (
	"fmt"
	"math"
	"time"

	"github.com/ethereum-optimism/monitorism/op-monitorism/faultproof_withdrawals/validator"
	"github.com/ethereum-optimism/optimism/op-service/metrics"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	lru "github.com/hashicorp/golang-lru"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

const (
	suspiciousEventsOnChallengerWinsGamesCacheSize = 1000
)

type State struct {
	logger          log.Logger
	nextL1Height    uint64
	latestL1Height  uint64
	initialL1Height uint64
	latestL2Height  uint64

	eventsProcessed      uint64 // This counts the events that we have taken care of, and we are aware of.
	withdrawalsProcessed uint64 // This counts the withdrawals that have being completed and processed and we are not tracking anymore. eventProcessed >= withdrawalsProcessed. withdrawalsProcessed does not includes potential attacks with games in progress.

	nodeConnectionFailures uint64
	nodeConnections        uint64

	// possible attacks detected

	// Forgeries detected on games that are already resolved
	potentialAttackOnDefenderWinsGames          map[common.Hash]*validator.EnrichedProvenWithdrawalEvent
	numberOfPotentialAttacksOnDefenderWinsGames uint64

	// Forgeries detected on games that are still in progress
	// Faultproof system should make them invalid
	potentialAttackOnInProgressGames         map[common.Hash]*validator.EnrichedProvenWithdrawalEvent
	numberOfPotentialAttackOnInProgressGames uint64

	// Suspicious events
	// It is unlikely that someone is going to use a withdrawal hash on a games that resolved with ChallengerWins. If this happens, maybe there is a bug somewhere in the UI used by the users or it is a malicious attack that failed
	suspiciousEventsOnChallengerWinsGames         *lru.Cache
	numberOfSuspiciousEventsOnChallengerWinsGames uint64

	provenWithdrawalValidator *validator.ProvenWithdrawalValidator
}

func NewState(logger log.Logger, provenWithdrawalValidator *validator.ProvenWithdrawalValidator) (*State, error) {
	nextL1Height, err := provenWithdrawalValidator.GetL1BlockNumber()
	if err != nil {
		return nil, fmt.Errorf("failed to get L1 block number: %w", err)
	}
	latestL1Height, err := provenWithdrawalValidator.GetL1BlockNumber()
	if err != nil {
		return nil, fmt.Errorf("failed to get L1 block number: %w", err)
	}
	latestL2Height, err := provenWithdrawalValidator.GetL2BlockNumber()
	if err != nil {
		return nil, fmt.Errorf("failed to get L2 block number: %w", err)
	}

	ret := State{
		potentialAttackOnDefenderWinsGames:          make(map[common.Hash]*validator.EnrichedProvenWithdrawalEvent),
		numberOfPotentialAttacksOnDefenderWinsGames: 0,
		suspiciousEventsOnChallengerWinsGames: func() *lru.Cache {
			cache, err := lru.New(suspiciousEventsOnChallengerWinsGamesCacheSize)
			if err != nil {
				logger.Error("Failed to create LRU cache", "error", err)
				return nil
			}
			return cache
		}(),
		numberOfSuspiciousEventsOnChallengerWinsGames: 0,

		potentialAttackOnInProgressGames:         make(map[common.Hash]*validator.EnrichedProvenWithdrawalEvent),
		numberOfPotentialAttackOnInProgressGames: 0,

		eventsProcessed: 0,

		withdrawalsProcessed:   0,
		nodeConnectionFailures: 0,
		nodeConnections:        0,

		nextL1Height:              nextL1Height,
		latestL1Height:            latestL1Height,
		initialL1Height:           nextL1Height,
		latestL2Height:            latestL2Height,
		logger:                    logger,
		provenWithdrawalValidator: provenWithdrawalValidator,
	}

	return &ret, nil
}

func (s *State) GetNodeConnectionFailures() uint64 {
	return s.provenWithdrawalValidator.L1Proxy.GetTotalConnectionErrors() + s.provenWithdrawalValidator.L2Proxy.GetTotalConnectionErrors()
}

func (s *State) GetNodeConnections() uint64 {
	return s.provenWithdrawalValidator.L1Proxy.GetTotalConnections() + s.provenWithdrawalValidator.L2Proxy.GetTotalConnections()
}

func (s *State) LogState() {
	blockToProcess, syncPercentage := s.GetPercentages()

	s.logger.Info("STATE:",
		"withdrawalsProcessed", fmt.Sprintf("%d", s.withdrawalsProcessed),

		"initialL1Height", fmt.Sprintf("%d", s.initialL1Height),
		"nextL1Height", fmt.Sprintf("%d", s.nextL1Height),
		"latestL1Height", fmt.Sprintf("%d", s.latestL1Height),
		"latestL2Height", fmt.Sprintf("%d", s.latestL2Height),
		"blockToProcess", fmt.Sprintf("%d", blockToProcess),
		"syncPercentage", fmt.Sprintf("%d%%", syncPercentage),

		"eventsProcessed", fmt.Sprintf("%d", s.eventsProcessed),
		"nodeConnectionFailures", fmt.Sprintf("%d", s.nodeConnectionFailures),
		"nodeConnections", fmt.Sprintf("%d", s.nodeConnections),
		"potentialAttackOnDefenderWinsGames", fmt.Sprintf("%d", s.numberOfPotentialAttacksOnDefenderWinsGames),
		"potentialAttackOnInProgressGames", fmt.Sprintf("%d", s.numberOfPotentialAttackOnInProgressGames),
		"suspiciousEventsOnChallengerWinsGames", fmt.Sprintf("%d", s.numberOfSuspiciousEventsOnChallengerWinsGames),
	)
}

func (s *State) IncrementWithdrawalsValidated(enrichedWithdrawalEvent *validator.EnrichedProvenWithdrawalEvent) {
	s.logger.Info("STATE WITHDRAWAL: valid", "TxHash", fmt.Sprintf("%v", enrichedWithdrawalEvent.Event.Raw.TxHash), "enrichedWithdrawalEvent", enrichedWithdrawalEvent)
	s.withdrawalsProcessed++
	enrichedWithdrawalEvent.ProcessedTimeStamp = float64(time.Now().Unix())
}

func (s *State) IncrementPotentialAttackOnDefenderWinsGames(enrichedWithdrawalEvent *validator.EnrichedProvenWithdrawalEvent) {
	key := enrichedWithdrawalEvent.Event.Raw.TxHash

	s.logger.Error("STATE WITHDRAWAL: is NOT valid, forgery detected", "TxHash", fmt.Sprintf("%v", enrichedWithdrawalEvent.Event.Raw.TxHash), "enrichedWithdrawalEvent", enrichedWithdrawalEvent)
	s.potentialAttackOnDefenderWinsGames[key] = enrichedWithdrawalEvent
	s.numberOfPotentialAttacksOnDefenderWinsGames++

	if _, ok := s.potentialAttackOnInProgressGames[key]; ok {
		s.logger.Error("STATE WITHDRAWAL: added to potential attacks. Removing from inProgress", "TxHash", fmt.Sprintf("%v", enrichedWithdrawalEvent.Event.Raw.TxHash), "enrichedWithdrawalEvent", enrichedWithdrawalEvent)
		delete(s.potentialAttackOnInProgressGames, key)
		s.numberOfPotentialAttackOnInProgressGames--
	}

	s.withdrawalsProcessed++
	enrichedWithdrawalEvent.ProcessedTimeStamp = float64(time.Now().Unix())

}

func (s *State) IncrementPotentialAttackOnInProgressGames(enrichedWithdrawalEvent *validator.EnrichedProvenWithdrawalEvent) {
	key := enrichedWithdrawalEvent.Event.Raw.TxHash
	// check if key already exists
	if _, ok := s.potentialAttackOnInProgressGames[key]; ok {
		s.logger.Error("STATE WITHDRAWAL:is NOT valid, game is still in progress", "TxHash", fmt.Sprintf("%v", enrichedWithdrawalEvent.Event.Raw.TxHash), "enrichedWithdrawalEvent", enrichedWithdrawalEvent)
	} else {
		s.logger.Error("STATE WITHDRAWAL:is NOT valid, game is still in progress. New game found In Progress", "TxHash", fmt.Sprintf("%v", enrichedWithdrawalEvent.Event.Raw.TxHash), "enrichedWithdrawalEvent", enrichedWithdrawalEvent)
		s.numberOfPotentialAttackOnInProgressGames++
		enrichedWithdrawalEvent.ProcessedTimeStamp = float64(time.Now().Unix())

	}

	// eventually update the map with the new enrichedWithdrawalEvent
	s.potentialAttackOnInProgressGames[key] = enrichedWithdrawalEvent
}

func (s *State) IncrementSuspiciousEventsOnChallengerWinsGames(enrichedWithdrawalEvent *validator.EnrichedProvenWithdrawalEvent) {
	key := enrichedWithdrawalEvent.Event.Raw.TxHash

	s.logger.Error("STATE WITHDRAWAL:is NOT valid, but the game is correctly resolved", "TxHash", fmt.Sprintf("%v", enrichedWithdrawalEvent.Event.Raw.TxHash), "enrichedWithdrawalEvent", enrichedWithdrawalEvent)
	s.suspiciousEventsOnChallengerWinsGames.Add(key, enrichedWithdrawalEvent)
	s.numberOfSuspiciousEventsOnChallengerWinsGames++

	if _, ok := s.potentialAttackOnInProgressGames[key]; ok {
		s.logger.Error("STATE WITHDRAWAL: added to suspicious attacks. Removing from inProgress", "TxHash", fmt.Sprintf("%v", enrichedWithdrawalEvent.Event.Raw.TxHash), "enrichedWithdrawalEvent", enrichedWithdrawalEvent)
		delete(s.potentialAttackOnInProgressGames, key)
		s.numberOfPotentialAttackOnInProgressGames--
	}

	s.withdrawalsProcessed++
	enrichedWithdrawalEvent.ProcessedTimeStamp = float64(time.Now().Unix())
}

func (s *State) GetPercentages() (uint64, uint64) {
	blockToProcess := s.latestL1Height - s.nextL1Height
	divisor := float64(s.latestL1Height) * 100
	//checking to avoid division by 0
	if divisor == 0 {
		return 0, 0
	}
	syncPercentage := uint64(math.Floor(100 - (float64(blockToProcess) / divisor)))
	return blockToProcess, syncPercentage
}

type Metrics struct {
	UpGauge              prometheus.Gauge
	InitialL1HeightGauge prometheus.Gauge
	NextL1HeightGauge    prometheus.Gauge
	LatestL1HeightGauge  prometheus.Gauge
	LatestL2HeightGauge  prometheus.Gauge

	EventsProcessedCounter      prometheus.Counter
	WithdrawalsProcessedCounter prometheus.Counter

	NodeConnectionFailuresCounter              prometheus.Counter
	NodeConnectionsCounter                     prometheus.Counter
	PotentialAttackOnDefenderWinsGamesGauge    prometheus.Gauge
	PotentialAttackOnInProgressGamesGauge      prometheus.Gauge
	SuspiciousEventsOnChallengerWinsGamesGauge prometheus.Gauge

	PotentialAttackOnDefenderWinsGamesGaugeVec    *prometheus.GaugeVec
	PotentialAttackOnInProgressGamesGaugeVec      *prometheus.GaugeVec
	SuspiciousEventsOnChallengerWinsGamesGaugeVec *prometheus.GaugeVec

	// Previous values for counters
	previousEventsProcessed        uint64
	previousWithdrawalsProcessed   uint64
	previousNodeConnectionFailures uint64
	previousNodeConnections        uint64
}

func (m *Metrics) String() string {
	upGaugeValue, _ := GetGaugeValue(m.UpGauge)
	initialL1HeightGaugeValue, _ := GetGaugeValue(m.InitialL1HeightGauge)
	nextL1HeightGaugeValue, _ := GetGaugeValue(m.NextL1HeightGauge)
	latestL1HeightGaugeValue, _ := GetGaugeValue(m.LatestL1HeightGauge)
	latestL2HeightGaugeValue, _ := GetGaugeValue(m.LatestL2HeightGauge)

	withdrawalsProcessedCounterValue, _ := GetCounterValue(m.WithdrawalsProcessedCounter)
	eventsProcessedCounterValue, _ := GetCounterValue(m.EventsProcessedCounter)

	nodeConnectionFailuresCounterValue, _ := GetCounterValue(m.NodeConnectionFailuresCounter)
	nodeConnectionsCounterValue, _ := GetCounterValue(m.NodeConnectionsCounter)

	potentialAttackOnDefenderWinsGamesGaugeValue, _ := GetGaugeValue(m.PotentialAttackOnDefenderWinsGamesGauge)
	potentialAttackOnInProgressGamesGaugeValue, _ := GetGaugeValue(m.PotentialAttackOnInProgressGamesGauge)

	forgeriesWithdrawalsEventsGaugeVecValue, _ := GetGaugeVecValue(m.PotentialAttackOnDefenderWinsGamesGaugeVec, prometheus.Labels{})
	invalidProposalWithdrawalsEventsGaugeVecValue, _ := GetGaugeVecValue(m.PotentialAttackOnInProgressGamesGaugeVec, prometheus.Labels{})

	return fmt.Sprintf(
		"Up: %d\nInitialL1HeightGauge: %d\nNextL1HeightGauge: %d\nLatestL1HeightGauge: %d\n latestL2HeightGaugeValue: %d\n eventsProcessedCounterValue: %d\nwithdrawalsProcessedCounterValue: %d\nnodeConnectionFailuresCounterValue: %d\nnodeConnectionsCounterValue: %d\n potentialAttackOnDefenderWinsGamesGaugeValue: %d\n potentialAttackOnInProgressGamesGaugeValue: %d\n  forgeriesWithdrawalsEventsGaugeVecValue: %d\n invalidProposalWithdrawalsEventsGaugeVecValue: %d\n previousEventsProcessed: %d\n previousWithdrawalsProcessed: %d\n previousNodeConnectionFailures: %d\n previousNodeConnections: %d\n",
		uint64(upGaugeValue),
		uint64(initialL1HeightGaugeValue),
		uint64(nextL1HeightGaugeValue),
		uint64(latestL1HeightGaugeValue),
		uint64(latestL2HeightGaugeValue),
		uint64(eventsProcessedCounterValue),
		uint64(withdrawalsProcessedCounterValue),
		uint64(nodeConnectionFailuresCounterValue),
		uint64(nodeConnectionsCounterValue),
		uint64(potentialAttackOnDefenderWinsGamesGaugeValue),
		uint64(potentialAttackOnInProgressGamesGaugeValue),
		uint64(forgeriesWithdrawalsEventsGaugeVecValue),
		uint64(invalidProposalWithdrawalsEventsGaugeVecValue),
		m.previousEventsProcessed,
		m.previousWithdrawalsProcessed,
		m.previousNodeConnectionFailures,
		m.previousNodeConnections,
	)
}

// Generic function to get the value of any prometheus.Counter
func GetCounterValue(counter prometheus.Counter) (float64, error) {
	metric := &dto.Metric{}
	err := counter.Write(metric)
	if err != nil {
		return 0, err
	}
	return metric.GetCounter().GetValue(), nil
}

// Generic function to get the value of any prometheus.Gauge
func GetGaugeValue(gauge prometheus.Gauge) (float64, error) {
	metric := &dto.Metric{}
	err := gauge.Write(metric)
	if err != nil {
		return 0, err
	}
	return metric.GetGauge().GetValue(), nil
}

// Function to get the value of a specific Gauge within a GaugeVec
func GetGaugeVecValue(gaugeVec *prometheus.GaugeVec, labels prometheus.Labels) (float64, error) {
	gauge, err := gaugeVec.GetMetricWith(labels)
	if err != nil {
		return 0, err
	}

	metric := &dto.Metric{}
	err = gauge.Write(metric)
	if err != nil {
		return 0, err
	}
	return metric.GetGauge().GetValue(), nil
}

func NewMetrics(m metrics.Factory) *Metrics {
	ret := &Metrics{
		UpGauge: m.NewGauge(prometheus.GaugeOpts{
			Namespace: MetricsNamespace,
			Name:      "up",
			Help:      "1 if the service is up and running, 0 otherwise",
		}),
		InitialL1HeightGauge: m.NewGauge(prometheus.GaugeOpts{
			Namespace: MetricsNamespace,
			Name:      "initial_l1_height",
			Help:      "Initial L1 Height",
		}),
		NextL1HeightGauge: m.NewGauge(prometheus.GaugeOpts{
			Namespace: MetricsNamespace,
			Name:      "next_l1_height",
			Help:      "Next L1 Height",
		}),
		LatestL1HeightGauge: m.NewGauge(prometheus.GaugeOpts{
			Namespace: MetricsNamespace,
			Name:      "latest_l1_height",
			Help:      "Latest L1 Height",
		}),
		LatestL2HeightGauge: m.NewGauge(prometheus.GaugeOpts{
			Namespace: MetricsNamespace,
			Name:      "latest_l2_height",
			Help:      "Latest L2 Height",
		}),
		EventsProcessedCounter: m.NewCounter(prometheus.CounterOpts{
			Namespace: MetricsNamespace,
			Name:      "events_processed_total",
			Help:      "Total number of events processed",
		}),
		WithdrawalsProcessedCounter: m.NewCounter(prometheus.CounterOpts{
			Namespace: MetricsNamespace,
			Name:      "withdrawals_processed_total",
			Help:      "Total number of withdrawals processed",
		}),
		NodeConnectionFailuresCounter: m.NewCounter(prometheus.CounterOpts{
			Namespace: MetricsNamespace,
			Name:      "node_connection_failures_total",
			Help:      "Total number of node connection failures",
		}),
		NodeConnectionsCounter: m.NewCounter(prometheus.CounterOpts{
			Namespace: MetricsNamespace,
			Name:      "node_connections_total",
			Help:      "Total number of node connections",
		}),
		PotentialAttackOnDefenderWinsGamesGauge: m.NewGauge(prometheus.GaugeOpts{
			Namespace: MetricsNamespace,
			Name:      "potential_attack_on_defender_wins_games_count",
			Help:      "Number of potential attacks on defender wins games",
		}),
		PotentialAttackOnInProgressGamesGauge: m.NewGauge(prometheus.GaugeOpts{
			Namespace: MetricsNamespace,
			Name:      "potential_attack_on_in_progress_games_count",
			Help:      "Number of potential attacks on in progress games",
		}),
		SuspiciousEventsOnChallengerWinsGamesGauge: m.NewGauge(prometheus.GaugeOpts{
			Namespace: MetricsNamespace,
			Name:      "suspicious_events_on_challenger_wins_games_count",
			Help:      "Number of suspicious events on challenger wins games",
		}),
		PotentialAttackOnDefenderWinsGamesGaugeVec: m.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: MetricsNamespace,
				Name:      "potential_attack_on_defender_wins_games_gauge_vec",
				Help:      "Information about potential attacks on defender wins games.",
			},
			[]string{"withdrawal_hash", "proof_submitter", "status", "TxHash", "TxL1BlockNumber", "ProxyAddress", "L2blockNumber", "RootClaim", "blacklisted", "withdrawal_hash_present", "enriched", "event_block_number", "event_tx_hash"},
		),
		PotentialAttackOnInProgressGamesGaugeVec: m.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: MetricsNamespace,
				Name:      "potential_attack_on_in_progress_games_gauge_vec",
				Help:      "Information about potential attacks on in progress games.",
			},
			[]string{"withdrawal_hash", "proof_submitter", "status", "TxHash", "TxL1BlockNumber", "ProxyAddress", "L2blockNumber", "RootClaim", "blacklisted", "withdrawal_hash_present", "enriched", "event_block_number", "event_tx_hash"},
		),
		SuspiciousEventsOnChallengerWinsGamesGaugeVec: m.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: MetricsNamespace,
				Name:      "suspicious_events_on_challenger_wins_games_info",
				Help:      "Information about suspicious events on challenger wins games.",
			},
			[]string{"withdrawal_hash", "proof_submitter", "status", "TxHash", "TxL1BlockNumber", "ProxyAddress", "L2blockNumber", "RootClaim", "blacklisted", "withdrawal_hash_present", "enriched", "event_block_number", "event_tx_hash"},
		),
	}

	return ret
}

func (m *Metrics) UpdateMetricsFromState(state *State) {

	// Set the up gauge to 1 to indicate the service is running
	m.UpGauge.Set(1)

	// Update Gauges
	m.InitialL1HeightGauge.Set(float64(state.initialL1Height))
	m.NextL1HeightGauge.Set(float64(state.nextL1Height))
	m.LatestL1HeightGauge.Set(float64(state.latestL1Height))
	m.LatestL2HeightGauge.Set(float64(state.latestL2Height))

	m.PotentialAttackOnDefenderWinsGamesGauge.Set(float64(state.numberOfPotentialAttacksOnDefenderWinsGames))
	m.PotentialAttackOnInProgressGamesGauge.Set(float64(state.numberOfPotentialAttackOnInProgressGames))
	m.SuspiciousEventsOnChallengerWinsGamesGauge.Set(float64(state.numberOfSuspiciousEventsOnChallengerWinsGames))

	// Update Counters by calculating deltas
	// Processed Withdrawals
	eventsProcessedDelta := state.eventsProcessed - m.previousEventsProcessed
	if eventsProcessedDelta > 0 {
		m.EventsProcessedCounter.Add(float64(eventsProcessedDelta))
	}
	m.previousEventsProcessed = state.eventsProcessed

	// Withdrawals Validated
	withdrawalsProcessedDelta := state.withdrawalsProcessed - m.previousWithdrawalsProcessed
	if withdrawalsProcessedDelta > 0 {
		m.WithdrawalsProcessedCounter.Add(float64(withdrawalsProcessedDelta))
	}
	m.previousWithdrawalsProcessed = state.withdrawalsProcessed

	// Node Connection Failures
	nodeConnectionFailuresDelta := state.GetNodeConnectionFailures() - m.previousNodeConnectionFailures
	if nodeConnectionFailuresDelta > 0 {
		m.NodeConnectionFailuresCounter.Add(float64(nodeConnectionFailuresDelta))
	}
	m.previousNodeConnectionFailures = state.GetNodeConnectionFailures()

	nodeConnectionsDelta := state.GetNodeConnections() - m.previousNodeConnections
	if nodeConnectionsDelta > 0 {
		m.NodeConnectionsCounter.Add(float64(nodeConnectionsDelta))
	}
	m.previousNodeConnections = state.GetNodeConnections()

	// Clear the previous values
	m.PotentialAttackOnDefenderWinsGamesGaugeVec.Reset()

	// Update metrics for forgeries withdrawals events
	for _, event := range state.potentialAttackOnDefenderWinsGames {
		withdrawalHash := common.BytesToHash(event.Event.WithdrawalHash[:]).Hex()
		proofSubmitter := event.Event.ProofSubmitter.String()
		status := event.DisputeGame.DisputeGameData.Status.String()

		m.PotentialAttackOnDefenderWinsGamesGaugeVec.WithLabelValues(
			withdrawalHash,
			proofSubmitter,
			status,
			fmt.Sprintf("%v", event.Event.Raw.TxHash),
			fmt.Sprintf("%v", event.Event.Raw.BlockNumber),
			fmt.Sprintf("%v", event.DisputeGame.DisputeGameData.ProxyAddress),
			fmt.Sprintf("%v", event.DisputeGame.DisputeGameData.L2blockNumber),
			fmt.Sprintf("%v", event.DisputeGame.DisputeGameData.RootClaim),
			fmt.Sprintf("%v", event.Blacklisted),
			fmt.Sprintf("%v", event.WithdrawalHashPresentOnL2),
			fmt.Sprintf("%v", event.Enriched),
			fmt.Sprintf("%v", event.Event.Raw.BlockNumber),
			event.Event.Raw.TxHash.String(),
		).Set(event.ProcessedTimeStamp) // Set the timestamp of when the event was processed
	}

	// Clear the previous values
	m.PotentialAttackOnInProgressGamesGaugeVec.Reset()

	// Update metrics for invalid proposal withdrawals events
	for _, event := range state.potentialAttackOnInProgressGames {
		withdrawalHash := common.BytesToHash(event.Event.WithdrawalHash[:]).Hex()
		proofSubmitter := event.Event.ProofSubmitter.String()
		status := event.DisputeGame.DisputeGameData.Status.String()

		m.PotentialAttackOnInProgressGamesGaugeVec.WithLabelValues(
			withdrawalHash,
			proofSubmitter,
			status,
			fmt.Sprintf("%v", event.Event.Raw.TxHash),
			fmt.Sprintf("%v", event.Event.Raw.BlockNumber),
			fmt.Sprintf("%v", event.DisputeGame.DisputeGameData.ProxyAddress),
			fmt.Sprintf("%v", event.DisputeGame.DisputeGameData.L2blockNumber),
			fmt.Sprintf("%v", event.DisputeGame.DisputeGameData.RootClaim),
			fmt.Sprintf("%v", event.Blacklisted),
			fmt.Sprintf("%v", event.WithdrawalHashPresentOnL2),
			fmt.Sprintf("%v", event.Enriched),
			fmt.Sprintf("%v", event.Event.Raw.BlockNumber),
			event.Event.Raw.TxHash.String(),
		).Set(event.ProcessedTimeStamp) // Set the timestamp of when the event was processed
	}

	// Clear the previous values
	m.SuspiciousEventsOnChallengerWinsGamesGaugeVec.Reset()
	// Update metrics for invalid proposal withdrawals events
	for _, key := range state.suspiciousEventsOnChallengerWinsGames.Keys() {
		enrichedEvent, ok := state.suspiciousEventsOnChallengerWinsGames.Get(key)
		if ok {
			event := enrichedEvent.(*validator.EnrichedProvenWithdrawalEvent)
			withdrawalHash := common.BytesToHash(event.Event.WithdrawalHash[:]).Hex()
			proofSubmitter := event.Event.ProofSubmitter.String()
			status := event.DisputeGame.DisputeGameData.Status.String()

			m.SuspiciousEventsOnChallengerWinsGamesGaugeVec.WithLabelValues(
				withdrawalHash,
				proofSubmitter,
				status,
				fmt.Sprintf("%v", event.Event.Raw.TxHash),
				fmt.Sprintf("%v", event.Event.Raw.BlockNumber),
				fmt.Sprintf("%v", event.DisputeGame.DisputeGameData.ProxyAddress),
				fmt.Sprintf("%v", event.DisputeGame.DisputeGameData.L2blockNumber),
				fmt.Sprintf("%v", event.DisputeGame.DisputeGameData.RootClaim),
				fmt.Sprintf("%v", event.Blacklisted),
				fmt.Sprintf("%v", event.WithdrawalHashPresentOnL2),
				fmt.Sprintf("%v", event.Enriched),
				fmt.Sprintf("%v", event.Event.Raw.BlockNumber),
				event.Event.Raw.TxHash.String(),
			).Set(event.ProcessedTimeStamp) // Set the timestamp of when the event was processed
		}
	}
}
