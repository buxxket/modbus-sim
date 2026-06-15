package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"log"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/goburrow/modbus"

	"github.com/tbrandon/mbserver"
)

const defaultConfigPath = "registers.json"

var registerAccessConfig *configManager
var currentLogMode = logModeNormal

type logMode int

const (
	logModeQuiet logMode = iota
	logModeNormal
	logModeVerbose
)

func (m logMode) enabled(min logMode) bool {
	return m >= min
}

func parseLogMode(value string) (logMode, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "quiet":
		return logModeQuiet, nil
	case "normal":
		return logModeNormal, nil
	case "verbose":
		return logModeVerbose, nil
	default:
		return logModeNormal, fmt.Errorf("invalid -log-mode %q (valid: quiet, normal, verbose)", value)
	}
}

type appOptions struct {
	logMode logMode
}

func parseOptions() (appOptions, error) {
	modeArg := flag.String("log-mode", "normal", "Logging mode: quiet, normal, verbose")
	flag.Parse()

	mode, err := parseLogMode(*modeArg)
	if err != nil {
		return appOptions{}, err
	}

	return appOptions{logMode: mode}, nil
}

type configuredRegister struct {
	Register int         `json:"register"`
	Type     string      `json:"type"`
	Value    interface{} `json:"value"`
}

type registerConfig struct {
	Registers        []configuredRegister `json:"registers"`
	HoldingRegisters []configuredRegister `json:"holding_registers"`
	InputRegisters   []configuredRegister `json:"input_registers"`
}

type configManager struct {
	path      string
	mu        sync.RWMutex
	holding   map[int]uint16
	input     map[int]uint16
	writtenHolding map[int]uint16
	writtenInput   map[int]uint16
	modTime   time.Time
	lastHash  uint64
	heartbeat time.Time
	lastCheck time.Time
}

func newConfigManager(path string) (*configManager, error) {
	mgr := &configManager{
		path:           path,
		writtenHolding: make(map[int]uint16),
		writtenInput:   make(map[int]uint16),
	}
	if err := mgr.loadConfig(); err != nil {
		return nil, err
	}
	return mgr, nil
}

func (m *configManager) refreshIfNeeded() {
	m.mu.RLock()
	if time.Since(m.lastCheck) < time.Second {
		m.mu.RUnlock()
		return
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()

	if time.Since(m.lastCheck) < time.Second {
		return
	}
	m.lastCheck = time.Now()

	data, err := os.ReadFile(m.path)
	if err != nil {
		log.Printf("Could not read register config %q: %v", m.path, err)
		return
	}

	newHash := configHash(data)
	if newHash == m.lastHash {
		if currentLogMode.enabled(logModeVerbose) && time.Since(m.heartbeat) >= 10*time.Second {
			m.heartbeat = time.Now()
			log.Printf("Config watcher active for %s (hash=%016x)", m.path, newHash)
		}
		return
	}
	oldHash := m.lastHash
	if currentLogMode.enabled(logModeNormal) {
		log.Printf("Detected register config change for %s: hash %016x -> %016x", m.path, oldHash, newHash)
	}

	var next registerConfig
	if err := json.Unmarshal(data, &next); err != nil {
		m.lastHash = newHash
		log.Printf("Could not parse register config %q: %v (keeping previous register map)", m.path, err)
		return
	}
	holding, input, err := buildRegisterMaps(next)
	if err != nil {
		m.lastHash = newHash
		log.Printf("Ignoring invalid register config %q: %v (keeping previous register map)", m.path, err)
		return
	}

	if stat, statErr := os.Stat(m.path); statErr == nil {
		m.modTime = stat.ModTime()
	}
	m.holding = holding
	m.input = input
	m.writtenHolding = make(map[int]uint16)
	m.writtenInput = make(map[int]uint16)
	m.lastHash = newHash
	m.heartbeat = time.Now()
	if currentLogMode.enabled(logModeNormal) {
		log.Printf("Reloaded register config from %s", m.path)
	}
}

func (m *configManager) loadConfig() error {
	stat, err := os.Stat(m.path)
	if err != nil {
		return fmt.Errorf("stat register config %q: %w", m.path, err)
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		return fmt.Errorf("read register config %q: %w", m.path, err)
	}
	var cfg registerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse register config %q: %w", m.path, err)
	}
	holding, input, err := buildRegisterMaps(cfg)
	if err != nil {
		return fmt.Errorf("validate register config %q: %w", m.path, err)
	}

	m.mu.Lock()
	m.holding = holding
	m.input = input
	m.writtenHolding = make(map[int]uint16)
	m.writtenInput = make(map[int]uint16)
	m.modTime = stat.ModTime()
	m.lastHash = configHash(data)
	m.heartbeat = time.Now()
	m.lastCheck = time.Now()
	m.mu.Unlock()

	return nil
}

func configHash(data []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(data)
	return h.Sum64()
}

func (m *configManager) readRange(function uint8, register int, endRegister int) ([]uint16, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	config := m.holding
	written := m.writtenHolding
	if function == modbus.FuncCodeReadInputRegisters {
		config = m.input
		written = m.writtenInput
	}

	result := make([]uint16, 0, endRegister-register)
	for addr := register; addr < endRegister; addr++ {
		var val uint16
		var ok bool
		if val, ok = written[addr]; !ok {
			val, ok = config[addr]
		}
		if !ok {
			val = 0
		}
		result = append(result, val)
	}
	return result, true
}

func (m *configManager) writeRange(function uint8, register int, values []uint16) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	config := m.holding
	written := m.writtenHolding
	if function == modbus.FuncCodeReadInputRegisters {
		config = m.input
		written = m.writtenInput
	}

	for i, val := range values {
		addr := register + i
		if _, ok := config[addr]; !ok {
			return false
		}
		written[addr] = val
	}
	return true
}

func buildRegisterMaps(cfg registerConfig) (map[int]uint16, map[int]uint16, error) {
	holding := map[int]uint16{}
	input := map[int]uint16{}

	for i, entry := range cfg.Registers {
		function, baseAddress, err := resolveRegister(entry.Register)
		if err != nil {
			return nil, nil, fmt.Errorf("registers[%d]: %w", i, err)
		}

		words, err := decodeRegisterWords(entry.Type, entry.Value)
		if err != nil {
			return nil, nil, fmt.Errorf("registers[%d]: %w", i, err)
		}

		for offset, word := range words {
			address := baseAddress + offset
			if address > 65535 {
				return nil, nil, fmt.Errorf("registers[%d]: mapped address out of range: %d", i, address)
			}
			if function == modbus.FuncCodeReadInputRegisters {
				input[address] = word
			} else {
				holding[address] = word
			}
		}
	}

	for i, entry := range cfg.HoldingRegisters {
		baseAddress, err := resolveScopedRegister(entry.Register, modbus.FuncCodeReadHoldingRegisters)
		if err != nil {
			return nil, nil, fmt.Errorf("holding_registers[%d]: %w", i, err)
		}

		words, err := decodeRegisterWords(entry.Type, entry.Value)
		if err != nil {
			return nil, nil, fmt.Errorf("holding_registers[%d]: %w", i, err)
		}

		for offset, word := range words {
			address := baseAddress + offset
			if address > 65535 {
				return nil, nil, fmt.Errorf("holding_registers[%d]: mapped address out of range: %d", i, address)
			}
			holding[address] = word
		}
	}

	for i, entry := range cfg.InputRegisters {
		baseAddress, err := resolveScopedRegister(entry.Register, modbus.FuncCodeReadInputRegisters)
		if err != nil {
			return nil, nil, fmt.Errorf("input_registers[%d]: %w", i, err)
		}

		words, err := decodeRegisterWords(entry.Type, entry.Value)
		if err != nil {
			return nil, nil, fmt.Errorf("input_registers[%d]: %w", i, err)
		}

		for offset, word := range words {
			address := baseAddress + offset
			if address > 65535 {
				return nil, nil, fmt.Errorf("input_registers[%d]: mapped address out of range: %d", i, address)
			}
			input[address] = word
		}
	}

	return holding, input, nil
}

func resolveScopedRegister(register int, function uint8) (int, error) {
	// if function == modbus.FuncCodeReadInputRegisters {
	// 	if register >= 30000 && register <= 39999 {
	// 		return register, nil
	// 	}
	// 	return 0, fmt.Errorf("register must be full notation 3xxxx for input, got %d", register)
	// }
	//
	// if register >= 40000 && register <= 49999 {
	// 	return register, nil
	// }
	// return 0, fmt.Errorf("register must be full notation 4xxxx for holding, got %d", register)
	return register, nil
}

func resolveRegister(register int) (uint8, int, error) {
	if register >= 30000 && register <= 39999 {
		return modbus.FuncCodeReadInputRegisters, register, nil
	}
	if register >= 40000 && register <= 49999 {
		return modbus.FuncCodeReadHoldingRegisters, register, nil
	}
	return 0, 0, fmt.Errorf("register must be 3xxxx or 4xxxx, got %d", register)
}

func decodeRegisterWords(typ string, value interface{}) ([]uint16, error) {
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "uint16":
		n, err := parseIntegerInRange(value, 0, 65535)
		if err != nil {
			return nil, fmt.Errorf("uint16 value invalid: %w", err)
		}
		return []uint16{uint16(n)}, nil
	case "int16":
		n, err := parseIntegerInRange(value, -32768, 32767)
		if err != nil {
			return nil, fmt.Errorf("int16 value invalid: %w", err)
		}
		return []uint16{uint16(int16(n))}, nil
	case "uint32":
		n, err := parseIntegerInRange(value, 0, 4294967295)
		if err != nil {
			return nil, fmt.Errorf("uint32 value invalid: %w", err)
		}
		u := uint32(n)
		return []uint16{uint16((u >> 16) & 0xffff), uint16(u & 0xffff)}, nil
	case "int32":
		n, err := parseIntegerInRange(value, -2147483648, 2147483647)
		if err != nil {
			return nil, fmt.Errorf("int32 value invalid: %w", err)
		}
		u := uint32(int32(n))
		return []uint16{uint16((u >> 16) & 0xffff), uint16(u & 0xffff)}, nil
	case "float32":
		f, err := parseFloat(value)
		if err != nil {
			return nil, fmt.Errorf("float32 value invalid: %w", err)
		}
		bits := math.Float32bits(float32(f))
		return []uint16{uint16((bits >> 16) & 0xffff), uint16(bits & 0xffff)}, nil
	default:
		return nil, fmt.Errorf("unsupported type %q", typ)
	}
}

func parseIntegerInRange(value interface{}, min int64, max int64) (int64, error) {
	n, ok := value.(float64)
	if !ok {
		return 0, fmt.Errorf("expected a number")
	}
	if math.Trunc(n) != n {
		return 0, fmt.Errorf("expected an integer, got %v", n)
	}
	v := int64(n)
	if v < min || v > max {
		return 0, fmt.Errorf("must be between %d and %d", min, max)
	}
	return v, nil
}

func parseFloat(value interface{}) (float64, error) {
	f, ok := value.(float64)
	if !ok {
		return 0, fmt.Errorf("expected a number")
	}
	return f, nil
}

func main() {
	opts, err := parseOptions()
	if err != nil {
		log.Printf("Error: %s", err)
		os.Exit(2)
	}

	if err := run(opts); err != nil {
		log.Printf("Error: %s", err)
		os.Exit(1)
	}
}

func run(opts appOptions) error {
	currentLogMode = opts.logMode

	serv := mbserver.NewServer()
	configPath := os.Getenv("REGISTER_CONFIG_PATH")
	if configPath == "" {
		configPath = defaultConfigPath
	}

	cfgMgr, err := newConfigManager(configPath)
	if err != nil {
		return err
	}
	registerAccessConfig = cfgMgr
	if currentLogMode.enabled(logModeNormal) {
		log.Printf("Register config loaded from %s", configPath)
	}

	serv.RegisterFunctionHandler(modbus.FuncCodeReadInputRegisters, ReadHoldingRegisters) //note this is a hack
	serv.RegisterFunctionHandler(modbus.FuncCodeReadHoldingRegisters, ReadHoldingRegisters)
	serv.RegisterFunctionHandler(modbus.FuncCodeWriteSingleRegister, WriteSingleRegister)
	serv.RegisterFunctionHandler(modbus.FuncCodeWriteMultipleRegisters, WriteMultipleRegisters)

	listenAddr := "0.0.0.0:502"

	if currentLogMode.enabled(logModeNormal) {
		log.Printf("Modbus Server listening on %s", listenAddr)
	}
	err = serv.ListenTCP(listenAddr)
	if err != nil {
		return err
	}
	defer serv.Close()

	// Keep config hot-reload checks active even when clients are idle.
	for {
		cfgMgr.refreshIfNeeded()
		time.Sleep(1 * time.Second)
	}
}

//func ReadInputRegisters(s *mbserver.Server, frame mbserver.Framer) ([]byte, *mbserver.Exception) {
//	register, numRegs, endRegister := registerAddressAndNumber(frame)
//	if endRegister > 65536 {
//		return []byte{}, &mbserver.IllegalDataAddress
//	}
//	return append([]byte{byte(numRegs * 2)}, mbserver.Uint16ToBytes(s.InputRegisters[register:endRegister])...), &mbserver.Success
//}

func ReadHoldingRegisters(_ *mbserver.Server, frame mbserver.Framer) ([]byte, *mbserver.Exception) {
	register, numRegs, endRegister := registerAddressAndNumber(frame)
	function := frame.GetFunction()
	if endRegister > 65536 {
		return []byte{}, &mbserver.IllegalDataAddress
	}

	if registerAccessConfig == nil {
		return []byte{}, &mbserver.IllegalDataAddress
	}

	registerAccessConfig.refreshIfNeeded()
	values, ok := registerAccessConfig.readRange(function, register, endRegister)
	if !ok {
		return []byte{}, &mbserver.IllegalDataAddress
	}

	if currentLogMode.enabled(logModeVerbose) {
		log.Printf("Read r:%d, count:%d", register, numRegs)
	}
	return append([]byte{byte(numRegs * 2)}, mbserver.Uint16ToBytes(values)...), &mbserver.Success
}

func WriteSingleRegister(_ *mbserver.Server, frame mbserver.Framer) ([]byte, *mbserver.Exception) {
	data := frame.GetData()
	register := int(binary.BigEndian.Uint16(data[0:2]))
	value := binary.BigEndian.Uint16(data[2:4])

	if registerAccessConfig == nil {
		return []byte{}, &mbserver.IllegalDataAddress
	}

	registerAccessConfig.refreshIfNeeded()
	if !registerAccessConfig.writeRange(modbus.FuncCodeReadHoldingRegisters, register, []uint16{value}) {
		return []byte{}, &mbserver.IllegalDataAddress
	}

	log.Printf("Write r:%d, value:%d", register, value)
	return data, &mbserver.Success
}

func WriteMultipleRegisters(_ *mbserver.Server, frame mbserver.Framer) ([]byte, *mbserver.Exception) {
	data := frame.GetData()
	register := int(binary.BigEndian.Uint16(data[0:2]))
	numRegs := int(binary.BigEndian.Uint16(data[2:4]))
	byteCount := int(data[4])

	if byteCount != numRegs*2 {
		return []byte{}, &mbserver.IllegalDataValue
	}

	values := make([]uint16, numRegs)
	for i := 0; i < numRegs; i++ {
		values[i] = binary.BigEndian.Uint16(data[5+i*2 : 7+i*2])
	}

	if registerAccessConfig == nil {
		return []byte{}, &mbserver.IllegalDataAddress
	}

	registerAccessConfig.refreshIfNeeded()
	if !registerAccessConfig.writeRange(modbus.FuncCodeReadHoldingRegisters, register, values) {
		return []byte{}, &mbserver.IllegalDataAddress
	}

	log.Printf("Write r:%d, count:%d", register, numRegs)
	return data[0:5], &mbserver.Success
}

func registerAddressAndNumber(frame mbserver.Framer) (register int, numRegs int, endRegister int) {
	data := frame.GetData()
	register = int(binary.BigEndian.Uint16(data[0:2]))
	numRegs = int(binary.BigEndian.Uint16(data[2:4]))
	endRegister = register + numRegs
	return register, numRegs, endRegister
}
