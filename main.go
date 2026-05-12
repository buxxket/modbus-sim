package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
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
	modTime   time.Time
	lastCheck time.Time
}

func newConfigManager(path string) (*configManager, error) {
	mgr := &configManager{path: path}
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

	stat, err := os.Stat(m.path)
	if err != nil {
		log.Printf("Could not stat register config %q: %v", m.path, err)
		return
	}
	if !stat.ModTime().After(m.modTime) {
		return
	}

	data, err := os.ReadFile(m.path)
	if err != nil {
		log.Printf("Could not read register config %q: %v", m.path, err)
		return
	}

	var next registerConfig
	if err := json.Unmarshal(data, &next); err != nil {
		log.Printf("Could not parse register config %q: %v", m.path, err)
		return
	}
	holding, input, err := buildRegisterMaps(next)
	if err != nil {
		log.Printf("Ignoring invalid register config %q: %v", m.path, err)
		return
	}

	m.holding = holding
	m.input = input
	m.modTime = stat.ModTime()
	log.Printf("Reloaded register config from %s", m.path)
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
	m.modTime = stat.ModTime()
	m.lastCheck = time.Now()
	m.mu.Unlock()

	return nil
}

func (m *configManager) readRange(function uint8, register int, endRegister int) ([]uint16, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	values := m.holding
	if function == modbus.FuncCodeReadInputRegisters {
		values = m.input
	}

	result := make([]uint16, 0, endRegister-register)
	for addr := register; addr < endRegister; addr++ {
		val, ok := values[addr]
		if !ok {
			return nil, false
		}
		result = append(result, val)
	}
	return result, true
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
	if function == modbus.FuncCodeReadInputRegisters {
		if register >= 30000 && register <= 39999 {
			return register, nil
		}
		return 0, fmt.Errorf("register must be full notation 3xxxx for input, got %d", register)
	}

	if register >= 40000 && register <= 49999 {
		return register, nil
	}
	return 0, fmt.Errorf("register must be full notation 4xxxx for holding, got %d", register)
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
	if err := run(); err != nil {
		log.Printf("Error: %s", err)
		os.Exit(1)
	}
}

func run() error {
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
	log.Printf("Register config loaded from %s", configPath)

	serv.RegisterFunctionHandler(modbus.FuncCodeReadInputRegisters, ReadHoldingRegisters) //note this is a hack
	serv.RegisterFunctionHandler(modbus.FuncCodeReadHoldingRegisters, ReadHoldingRegisters)

	listenAddr := "0.0.0.0:1502"

	log.Printf("Modbus Server listening on %s", listenAddr)
	err = serv.ListenTCP(listenAddr)
	if err != nil {
		return err
	}
	defer serv.Close()

	// Wait forever
	for {
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

	log.Printf("Read r:%d, count:%d", register, numRegs)
	return append([]byte{byte(numRegs * 2)}, mbserver.Uint16ToBytes(values)...), &mbserver.Success
}

func registerAddressAndNumber(frame mbserver.Framer) (register int, numRegs int, endRegister int) {
	data := frame.GetData()
	register = int(binary.BigEndian.Uint16(data[0:2]))
	numRegs = int(binary.BigEndian.Uint16(data[2:4]))
	endRegister = register + numRegs
	return register, numRegs, endRegister
}
