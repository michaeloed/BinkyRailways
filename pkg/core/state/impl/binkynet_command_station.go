// Copyright 2021-2022 Ewout Prangsma
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Author Ewout Prangsma
//

package impl

import (
	"context"
	"fmt"
	"time"

	apiutil "github.com/binkynet/BinkyNet/apis/util"
	bn "github.com/binkynet/BinkyNet/apis/v1"
	lwsvc "github.com/binkynet/LocalWorker/pkg/service"
	"github.com/binkynet/NetManager/service"
	"github.com/binkynet/NetManager/service/manager"
	"github.com/binkynet/NetManager/service/server"
	"golang.org/x/sync/errgroup"

	"github.com/binkyrailways/BinkyRailways/pkg/core/model"
	"github.com/binkyrailways/BinkyRailways/pkg/core/state"
	"github.com/binkyrailways/BinkyRailways/pkg/core/util"
)

// binkyNetCommandStation implements the BinkyNetCommandStation.
type binkyNetCommandStation struct {
	commandStation

	power boolProperty

	reconfigureQueue    chan string
	manager             manager.Manager
	service             service.Service
	server              server.Server
	cancel              context.CancelFunc
	binaryOutputCounter map[bn.ObjectAddress]int
}

const (
	onActualTimeout       = time.Millisecond
	netManagerChanTimeout = time.Second
	dccModuleID           = "dcc"
)

// Create a new entity
func newBinkyNetCommandStation(en model.BinkyNetCommandStation, railway Railway) CommandStation {
	cs := &binkyNetCommandStation{
		commandStation:      newCommandStation(en, railway, false),
		binaryOutputCounter: make(map[bn.ObjectAddress]int),
	}
	cs.log = cs.log.With().Str("component", "bncs").Logger()
	cs.power.Configure("power", cs, nil, railway, railway)
	cs.power.SubscribeRequestChanges(cs.sendPower)
	return cs
}

// getCommandStation returns the entity as LocoBufferCommandStation.
func (cs *binkyNetCommandStation) getCommandStation() model.BinkyNetCommandStation {
	return cs.GetEntity().(model.BinkyNetCommandStation)
}

// Try to prepare the entity for use.
// Returns nil when the entity is successfully prepared,
// returns an error otherwise.
func (cs *binkyNetCommandStation) TryPrepareForUse(ctx context.Context, _ state.UserInterface, _ state.Persistence) error {
	log := cs.log
	var err error
	serverHost, err := util.FindServerHostAddress(cs.getCommandStation().GetServerHost())
	if err != nil {
		return fmt.Errorf("failed to find server host: %w", err)
	}
	cs.reconfigureQueue = make(chan string, 64)
	registry := newBinkyNetConfigRegistry(cs.getCommandStation().GetLocalWorkers(),
		cs.onUnknownLocalWorker, cs.isObjectUsed)
	cs.manager, err = manager.New(manager.Dependencies{
		Log:              log,
		ReconfigureQueue: cs.reconfigureQueue,
	})
	if err != nil {
		return err
	}
	registry.ForEach(ctx, func(ctx context.Context, lw bn.LocalWorker) error {
		lw.Request.Hash = lw.GetRequest().Sha1()
		log.Info().Str("hash", lw.GetRequest().GetHash()).Msg("Setting local worker request")
		return cs.manager.SetLocalWorkerRequest(ctx, lw)
	})
	//registry
	cs.service, err = service.NewService(service.Config{
		RequiredWorkerVersion: cs.getCommandStation().GetRequiredWorkerVersion(),
	}, service.Dependencies{
		Log:     log,
		Manager: cs.manager,
	})
	if err != nil {
		return err
	}
	cs.server, err = server.NewServer(server.Config{
		Host:     serverHost,
		GRPCPort: cs.getCommandStation().GetGRPCPort(),
	}, cs.service, log)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	cs.cancel = cancel
	g, ctx := errgroup.WithContext(ctx)
	ctx = bn.WithServiceInfoHost(ctx, serverHost)
	g.Go(func() (result error) {
		defer func() {
			log.Info().Err(result).Msg("Binky NetManager ended")
		}()
		err := cs.manager.Run(ctx)
		result = apiutil.ContextCanceledOrUnexpected(ctx, err, "Binky NetManager")
		return
	})
	g.Go(func() (result error) {
		defer func() {
			log.Info().Err(result).Msg("Binky NetManager.Server ended")
		}()
		err := cs.server.Run(ctx)
		result = apiutil.ContextCanceledOrUnexpected(ctx, err, "Binky NetManager.Server")
		return
	})
	g.Go(func() error { cs.runSendLocSpeedAndDirection(ctx); return nil })
	g.Go(func() error { cs.runSendOutputActive(ctx); return nil })
	g.Go(func() error { cs.runSendSwitchDirection(ctx); return nil })
	g.Go(func() error {
		updates, cancel := cs.manager.SubscribeLocalWorkerActuals(true, netManagerChanTimeout, "")
		defer cancel()
		for {
			select {
			case <-updates:
				cs.railway.Exclusive(ctx, onActualTimeout, "onLocalWorkerActualChange", func(ctx context.Context) error {
					/*_, remoteAddr, _, _ := cs.manager.GetLocalWorkerInfo(lw.GetId())
					actual := lw.GetActual()
					name := actual.GetId()
					cs.railway.RegisterScrapeTarget(name, remoteAddr, int(actual.GetMetricsPort()), actual.GetMetricsSecure())
					*/
					cs.railway.Send(state.ActualStateChangedEvent{
						Subject: cs,
					})
					return nil
				})
			case <-ctx.Done():
				return nil
			}
		}
	})
	g.Go(func() error {
		actuals, cancel := cs.manager.SubscribePowerActuals(true, netManagerChanTimeout)
		defer cancel()
		for {
			select {
			case actual := <-actuals:
				cs.railway.Exclusive(ctx, onActualTimeout, "onPowerActualChange", func(ctx context.Context) error {
					cs.onPowerActual(ctx, actual)
					return nil
				})
			case <-ctx.Done():
				return nil
			}
		}
	})
	g.Go(func() error {
		actuals, cancel := cs.manager.SubscribeLocActuals(true, netManagerChanTimeout)
		defer cancel()
		for {
			select {
			case actual := <-actuals:
				cs.railway.Exclusive(ctx, onActualTimeout, "onLocActualChange", func(ctx context.Context) error {
					cs.onLocActual(ctx, actual)
					return nil
				})
			case <-ctx.Done():
				return nil
			}
		}
	})
	g.Go(func() error {
		actuals, cancel := cs.manager.SubscribeOutputActuals(true, netManagerChanTimeout, "")
		defer cancel()
		for {
			select {
			case actual := <-actuals:
				cs.railway.Exclusive(ctx, onActualTimeout, "onOutputActualChange", func(ctx context.Context) error {
					log.Debug().
						Str("address", string(actual.Address)).
						Msg("output actual update")
					cs.onOutputActual(ctx, actual)
					return nil
				})
			case <-ctx.Done():
				return nil
			}
		}
	})
	g.Go(func() error {
		actuals, cancel := cs.manager.SubscribeSensorActuals(true, netManagerChanTimeout, "")
		defer cancel()
		for {
			select {
			case actual := <-actuals:
				cs.railway.Exclusive(ctx, onActualTimeout, "onSensorActualChange", func(ctx context.Context) error {
					cs.onSensorActual(ctx, actual)
					return nil
				})
			case <-ctx.Done():
				return nil
			}
		}
	})

	g.Go(func() error {
		actuals, cancel := cs.manager.SubscribeSwitchActuals(true, netManagerChanTimeout, "")
		defer cancel()
		for {
			select {
			case actual := <-actuals:
				cs.railway.Exclusive(ctx, onActualTimeout, "onSwitchActualChange", func(ctx context.Context) error {
					cs.onSwitchActual(ctx, actual)
					return nil
				})
			case <-ctx.Done():
				return nil
			}
		}
	})

	basePort := 21010
	lokiLogger := lwsvc.NewLokiLogger()
	cs.getCommandStation().GetLocalWorkers().ForEach(func(bnlw model.BinkyNetLocalWorker) {
		if bnlw.GetLocalWorkerType() != model.BinkynetLocalWorkerTypeEsphome {
			return
		}
		// Build loki logger if needed
		// Run esphome local worker
		vlw := newBinkyNetEsphomeLocalWorker(log, bnlw, lokiLogger, basePort, serverHost, cs.getCommandStation().GetGRPCPort())
		basePort += 10
		g.Go(func() error { return vlw.Run(ctx) })
	})

	return nil
}

// Wrap up the preparation fase.
func (cs *binkyNetCommandStation) FinalizePrepare(ctx context.Context) {
	// TODO
}

// Enable/disable power on the railway
func (cs *binkyNetCommandStation) GetPower() state.BoolProperty {
	return &cs.power
}

// Has the command station not send or received anything for a while.
func (cs *binkyNetCommandStation) GetIdle(context.Context) bool {
	return true // TODO
}

// Send the requested power state.
func (cs *binkyNetCommandStation) sendPower(ctx context.Context, enabled bool) {
	fmt.Printf("sendPower(%v)\n", enabled)
	cs.manager.SetPowerRequest(bn.PowerState{
		Enabled: enabled,
	})
}

// Send the state of the binary output towards the railway.
func (cs *binkyNetCommandStation) onPowerActual(ctx context.Context, actual bn.Power) {
	// Update (internal) actual status
	enabled := actual.GetActual().GetEnabled()
	cs.power.SetActual(ctx, enabled)
	// If actual power turned off, also turn off the requested power.
	if !enabled {
		//cs.power.SetRequested(ctx, enabled)
	}
}

// Send the speed and direction of the given loc towards the railway.
func (cs *binkyNetCommandStation) SendLocSpeedAndDirection(ctx context.Context, loc state.Loc) {
	if !loc.GetEnabled() {
		return
	}

	addr := cs.createObjectAddress(loc.GetAddress(ctx))
	direction := bn.LocDirection_FORWARD
	if loc.GetDirection().GetRequested(ctx) == state.LocDirectionReverse {
		direction = bn.LocDirection_REVERSE
	}
	cs.manager.SetLocRequest(bn.Loc{
		Address: addr,
		Request: &bn.LocState{
			Speed:      int32(loc.GetSpeedInSteps().GetRequested(ctx)),
			SpeedSteps: int32(loc.GetSpeedSteps(ctx)),
			Direction:  direction,
			Functions: map[int32]bool{
				0: loc.GetF0().GetRequested(ctx),
				// TODO other functions
			},
		},
	})
}

// Update the state from the railway in our memory state
func (cs *binkyNetCommandStation) onLocActual(ctx context.Context, actual bn.Loc) {
	objAddr := actual.GetAddress()
	found := false
	cs.ForEachLoc(func(loc state.Loc) {
		if isAddressEqual(loc.GetAddress(ctx), objAddr) {
			found = true
			changed := false
			direction := state.LocDirectionForward
			if actual.GetActual().GetDirection() == bn.LocDirection_REVERSE {
				direction = state.LocDirectionReverse
			}
			if c, _ := loc.GetDirection().SetActual(ctx, direction); c {
				changed = true
			}
			if c, _ := loc.GetSpeedInSteps().SetActual(ctx, int(actual.GetActual().GetSpeed())); c {
				changed = true
			}
			if c, _ := loc.GetF0().SetActual(ctx, actual.Actual.GetFunctions()[0]); c {
				changed = true
			}
			// TODO other functions
			if changed {
				cs.log.Debug().
					Interface("addr", objAddr).
					Msg("Updated loc actual")
			}
		}
	})
	if !found {
		cs.log.Info().
			Interface("addr", objAddr).
			Msg("Got unexpected loc actual")
	}
}

// Send the state of the binary output towards the railway.
func (cs *binkyNetCommandStation) SendOutputActive(ctx context.Context, bo state.BinaryOutput) {
	cs.sendOutputActive(ctx, bo, false)
}

// Send the state of the binary output towards the railway.
func (cs *binkyNetCommandStation) sendOutputActive(ctx context.Context, bo state.BinaryOutput, currentOnly bool) {
	addr := cs.createObjectAddress(bo.GetAddress())

	switch bo.GetBinaryOutputType() {
	case model.BinaryOutputTypeDefault:
		value := int32(0)
		if bo.GetActive().GetRequested(ctx) {
			value = 1
		}
		requestedOutputValueGauge.WithLabelValues(string(addr)).Set(float64(value))
		cs.manager.SetOutputRequest(bn.Output{
			Address: addr,
			Request: &bn.OutputState{
				Value: value,
			},
		})
	case model.BinaryOutputTypeTrackInverter:
		// Record counter
		cnt := cs.binaryOutputCounter[addr] + 1
		cs.binaryOutputCounter[addr] = cnt

		// Disconnect first
		value := int32(bn.TrackInverterStateNotConnected)
		requestedOutputValueGauge.WithLabelValues(string(addr)).Set(float64(value))
		cs.manager.SetOutputRequest(bn.Output{
			Address: addr,
			Request: &bn.OutputState{
				Value: value,
			},
		})

		// Do not block exclusive, so perform activation
		// after 100ms async waiting.
		util.Delayed(time.Millisecond*100, cs.railway, setRequestTimeout, "trackInvert.Activate", func(ctx context.Context) error {
			// Check counter
			if cs.binaryOutputCounter[addr] != cnt {
				// Counter changes, do not change
				cs.log.Info().
					Str("address", string(addr)).
					Msg("TrackInverter counter changed, skip activating inverter")
				return nil
			}
			// Reconnect to selected value
			value := bn.TrackInverterStateDefault
			if !bo.GetActive().GetRequested(ctx) {
				value = bn.TrackInverterStateReverse
			}
			requestedOutputValueGauge.WithLabelValues(string(addr)).Set(float64(value))
			cs.manager.SetOutputRequest(bn.Output{
				Address: addr,
				Request: &bn.OutputState{
					Value: int32(value),
				},
			})
			return nil
		})
	}
}

// Send the state of the binary output towards the railway.
func (cs *binkyNetCommandStation) onOutputActual(ctx context.Context, actual bn.Output) {
	objAddr := actual.GetAddress()
	cs.log.Debug().
		Int32("value", actual.GetActual().GetValue()).
		Str("addr", string(objAddr)).
		Msg("Got output actual")
	cs.ForEachOutput(func(output state.Output) {
		if bo, ok := output.(state.BinaryOutput); ok {
			if isAddressEqual(bo.GetAddress(), objAddr) {
				switch bo.GetBinaryOutputType() {
				case model.BinaryOutputTypeDefault:
					bo.GetActive().SetActual(ctx, actual.GetActual().GetValue() != 0)
				case model.BinaryOutputTypeTrackInverter:
					cs.log.Debug().
						Int32("value", actual.GetActual().GetValue()).
						Msg("Got track-inverter actual")
					switch actual.GetActual().GetValue() {
					case int32(bn.TrackInverterStateNotConnected):
						// Do nothing, transition in progress
					case int32(bn.TrackInverterStateDefault):
						bo.GetActive().SetActual(ctx, true)
					case int32(bn.TrackInverterStateReverse):
						bo.GetActive().SetActual(ctx, false)
					}
				}
			}
		}
	})
}

// Process the update of a sensor on the track
func (cs *binkyNetCommandStation) onSensorActual(ctx context.Context, actual bn.Sensor) {
	objAddr := actual.GetAddress()
	found := false
	notFound := 0
	cs.ForEachSensor(func(sensor state.Sensor) {
		if isAddressEqual(sensor.GetAddress(), objAddr) {
			sensor.GetActive().SetActual(ctx, actual.GetActual().GetValue() != 0)
			found = true
		} else {
			notFound++
		}
	})
	if !found {
		cs.log.Info().
			Str("address", string(objAddr)).
			Int32("value", actual.GetActual().GetValue()).
			Int("sensors", notFound).
			Msg("Unknown sensor detected")
	}
}

// Send the state of the binary switch towards the railway.
func (cs *binkyNetCommandStation) onSwitchActual(ctx context.Context, actual bn.Switch) {
	objAddr := actual.GetAddress()
	cs.ForEachJunction(func(output state.Junction) {
		if bo, ok := output.(state.Switch); ok {
			if isAddressEqual(bo.GetAddress(), objAddr) {
				switch actual.GetActual().GetDirection() {
				case bn.SwitchDirection_STRAIGHT:
					bo.GetDirection().SetActual(ctx, model.SwitchDirectionStraight)
				case bn.SwitchDirection_OFF:
					bo.GetDirection().SetActual(ctx, model.SwitchDirectionOff)
				}
			}
		}
	})
}

// Send the direction of the given switch towards the railway.
func (cs *binkyNetCommandStation) SendSwitchDirection(ctx context.Context, sw state.Switch) {
	cs.sendSwitchDirection(ctx, sw, false)
}

// Send the direction of the given switch towards the railway.
func (cs *binkyNetCommandStation) sendSwitchDirection(ctx context.Context, sw state.Switch, isRepeat bool) {
	addr := cs.createObjectAddress(sw.GetAddress())
	log := cs.log.With().Str("address", string(addr)).Logger()
	var direction bn.SwitchDirection
	switch dir := sw.GetDirection().GetRequested(ctx); dir {
	case model.SwitchDirectionStraight:
		direction = bn.SwitchDirection_STRAIGHT
	case model.SwitchDirectionOff:
		direction = bn.SwitchDirection_OFF
	default:
		// Unknown direction
		log.Error().
			Str("direction", string(dir)).
			Msg("Invalid switch direction: %w")
		return
	}
	isLocked := sw.GetLockedBy(ctx) != nil
	requestedSwitchDirectionGauge.WithLabelValues(string(addr)).Set(float64(direction))
	sendSwitchDirectionCounter.WithLabelValues(string(addr)).Inc()
	start := time.Now()
	cs.manager.SetSwitchRequest(bn.Switch{
		Address: addr,
		Request: &bn.SwitchState{
			Direction: direction,
			IsUsed:    isLocked,
		},
	})
	if !isRepeat {
		log.Debug().
			Int32("direction", int32(direction)).
			Bool("isUsed", isLocked).
			Dur("duration", time.Since(start)).
			Msg("Sent switch direction")
	}
}

// Send the position of the given turntable towards the railway.
//void SendTurnTablePosition(ITurnTableState turnTable);

// Send an event about the given unknown worker
func (cs *binkyNetCommandStation) onUnknownLocalWorker(hardwareID string) {
	if sender := cs.GetRailwayImpl(); sender != nil {
		sender.Send(state.UnknownBinkyNetLocalWorkerEvent{
			HardwareID: hardwareID,
		})
	}
}

// Trigger discovery of locally attached devices on local worker
func (cs *binkyNetCommandStation) TriggerDiscover(ctx context.Context, hardwareID string) error {
	// Translate alias into hardware ID (if needed)
	cs.getCommandStation().GetLocalWorkers().ForEach(func(lw model.BinkyNetLocalWorker) {
		if hardwareID == lw.GetAlias() {
			hardwareID = lw.GetHardwareID()
		}
	})

	go func() {
		log := cs.log.With().Str("hardware_id", hardwareID).Logger()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		log.Info().Msg("Trigger discover...")
		result, err := cs.manager.Discover(ctx, hardwareID)
		if err != nil {
			log.Warn().Err(err).Msg("Discover failed")
		} else {
			log.Info().Strs("addresses", result.GetAddresses()).Msg("Discover result")
		}
	}()
	return nil
}

// Close the commandstation
func (cs *binkyNetCommandStation) Close(ctx context.Context) {
	cancel, reconfigureQueue := cs.cancel, cs.reconfigureQueue
	cs.cancel = nil
	cs.reconfigureQueue = nil
	if cancel != nil {
		cancel()
	}
	if reconfigureQueue != nil {
		close(reconfigureQueue)
	}
	// TODO
}

// Iterate over all hardware modules this command station is in control of.
func (cs *binkyNetCommandStation) ForEachHardwareModule(cb func(state.HardwareModule)) {
	// Collect all defined local workers
	lws := cs.getCommandStation().GetLocalWorkers()
	localWorkers := make([]model.BinkyNetLocalWorker, 0, lws.GetCount())
	lws.ForEach(func(lw model.BinkyNetLocalWorker) {
		localWorkers = append(localWorkers, lw)
	})
	getLW := func(id string) model.BinkyNetLocalWorker {
		for _, lw := range localWorkers {
			if lw.GetAlias() == id || lw.GetHardwareID() == id {
				return lw
			}
		}
		return nil
	}

	// Return all local workers
	visited := make(map[string]struct{})
	for _, info := range cs.manager.GetAllLocalWorkers() {
		lwm := binkyNetLocalWorkerModule{
			ID:      info.GetId(),
			Manager: cs.manager,
		}
		if lw := getLW(info.GetId()); lw != nil {
			unconfigured := len(info.GetUnconfiguredObjectIds())
			if unconfigured > 0 {
				lwm.ErrorMessages = append(lwm.ErrorMessages, fmt.Sprintf("%d unconfigured objects", unconfigured))
			}
		} else if info.GetId() == dccModuleID {
			// DCC module is well known
		} else {
			lwm.ErrorMessages = []string{"Undefined local worker"}
		}
		visited[info.GetId()] = struct{}{}
		cb(&lwm)
	}
	// Return all declared local workers that were not yet returned
	for _, lw := range localWorkers {
		if _, found := visited[lw.GetAlias()]; !found {
			if _, found := visited[lw.GetHardwareID()]; !found {
				lwm := binkyNetLocalWorkerModule{
					ID:            lw.GetAlias(),
					Manager:       cs.manager,
					ErrorMessages: []string{"Local worker not sending pings"},
				}
				cb(&lwm)
			}
		}
	}
}

// Request a reset of hardware module with given ID
func (cs *binkyNetCommandStation) ResetHardwareModule(ctx context.Context, id string) error {
	cs.manager.RequestResetLocalWorker(ctx, id)
	return nil
}

// createObjectAddress converts a model address into a BinkyNet object address.
func (cs *binkyNetCommandStation) createObjectAddress(addr model.Address) bn.ObjectAddress {
	return bn.ObjectAddress(addr.Value)
}

// runSendLocSpeedAndDirection keeps sending the speed & direction of all locs at regular
// intervals.
func (cs *binkyNetCommandStation) runSendLocSpeedAndDirection(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			// Context canceled
			return
		case <-time.After(time.Second * 2):
			// Send again
		}
		for _, loc := range cs.locs {
			if loc.GetEnabled() {
				cs.SendLocSpeedAndDirection(ctx, loc)
			}
		}
	}
}

// runSendOutputActive keeps sending the active state of all outputs at regular
// intervals.
func (cs *binkyNetCommandStation) runSendOutputActive(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			// Context canceled
			return
		case <-time.After(time.Second * 2):
			// Send again
		}
		for _, output := range cs.outputs {
			if bo, ok := output.(state.BinaryOutput); ok {
				if bo.GetBinaryOutputType() != model.BinaryOutputTypeTrackInverter {
					cs.SendOutputActive(ctx, bo)
				}
			}
		}
	}
}

// runSendSwitchDirection keeps sending the direction of all switches at regular
// intervals.
func (cs *binkyNetCommandStation) runSendSwitchDirection(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			// Context canceled
			return
		case <-time.After(time.Second * 2):
			// Send again
		}
		maxDuration := (time.Millisecond * 25) * time.Duration(len(cs.junctions))
		start := time.Now()
		for _, junction := range cs.junctions {
			if sw, ok := junction.(state.Switch); ok {
				cs.sendSwitchDirection(ctx, sw, true)
			}
		}
		if duration := time.Since(start); duration > maxDuration {
			cs.log.Warn().
				Dur("duration", duration).
				Dur("max", maxDuration).
				Int("count", len(cs.junctions)).
				Msg("refresh of switch direction took too long")
		}
	}
}

// Determine if the object is to be included in the configuration
func (cs *binkyNetCommandStation) isObjectUsed(obj model.BinkyNetObject) bool {
	if !cs.getCommandStation().GetExcludeUnUsedObjects() {
		// We do not exclude anythin
		return true
	}
	used := false
	rw := cs.railway.GetModel()
	objAddr := bn.JoinModuleLocal(obj.GetLocalWorker().GetAlias(), string(obj.GetObjectID()))
	rw.GetModules().ForEach(func(mr model.ModuleRef) {
		if module, err := mr.TryResolve(); err == nil {
			module.ForEachAddressUsage(func(au model.AddressUsage) {
				if isAddressEqual(au.Address, objAddr) {
					used = true
				}
			})
		}
	})
	return used
}

// isAddressEqual returns true if the given addresses are the same
func isAddressEqual(modelAddr model.Address, objAddr bn.ObjectAddress) bool {
	// Try local addresses first
	if modelAddr.Value == string(objAddr) {
		return true
	}
	// Check global address
	modelObjAddr := bn.ObjectAddress(modelAddr.Value)
	if modelObjAddr.IsGlobal() {
		_, modelLocalAddr, _ := bn.SplitAddress(modelObjAddr)
		_, objLocalAddr, _ := bn.SplitAddress(objAddr)
		return modelLocalAddr == objLocalAddr
	}
	return false
}
