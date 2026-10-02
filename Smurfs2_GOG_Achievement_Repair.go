package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	toolVersion      = "1.1.2"
	productID        = "1906012961"
	expectedClientID = "56511167488088821"
	fallbackBuildID  = "56997659418857909"

	galaxyClientID     = "46899977096215655"
	galaxyClientSecret = "9d85c43b1482497dbbce61f6e4aa173a433796eeae2ca8c5f6129f2dc4de46d9"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}
var logFile *os.File

type authResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	UserID       string `json:"user_id"`
}

type buildDetails struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

type productData struct {
	Builds []struct {
		ID            int64  `json:"id"`
		DatePublished string `json:"date_published"`
		Listed        bool   `json:"listed"`
	} `json:"builds"`
}

type achievement struct {
	AchievementID  string  `json:"achievement_id"`
	AchievementKey string  `json:"achievement_key"`
	Name           string  `json:"name"`
	Description    string  `json:"description"`
	DateUnlocked   *string `json:"date_unlocked"`
}

type achievementsResponse struct {
	Items []achievement `json:"items"`
}

// ---------------- GVAS reader ----------------

type gvasReader struct {
	b []byte
	o int
}

func (r *gvasReader) need(n int) error {
	if n < 0 || r.o < 0 || r.o+n > len(r.b) {
		return fmt.Errorf("unexpected end of save at 0x%X (need %d bytes)", r.o, n)
	}
	return nil
}

func (r *gvasReader) u8() (byte, error) {
	if err := r.need(1); err != nil {
		return 0, err
	}
	v := r.b[r.o]
	r.o++
	return v, nil
}

func (r *gvasReader) i32() (int32, error) {
	if err := r.need(4); err != nil {
		return 0, err
	}
	v := int32(binary.LittleEndian.Uint32(r.b[r.o : r.o+4]))
	r.o += 4
	return v, nil
}

func (r *gvasReader) f32() (float32, error) {
	if err := r.need(4); err != nil {
		return 0, err
	}
	v := math.Float32frombits(binary.LittleEndian.Uint32(r.b[r.o : r.o+4]))
	r.o += 4
	return v, nil
}

func (r *gvasReader) raw(n int) ([]byte, error) {
	if err := r.need(n); err != nil {
		return nil, err
	}
	v := r.b[r.o : r.o+n]
	r.o += n
	return v, nil
}

func (r *gvasReader) fstr() (string, error) {
	n32, err := r.i32()
	if err != nil {
		return "", err
	}
	if n32 == 0 {
		return "", nil
	}
	if n32 > 0 {
		n := int(n32)
		raw, err := r.raw(n)
		if err != nil {
			return "", err
		}
		if n == 0 {
			return "", nil
		}
		if raw[n-1] == 0 {
			raw = raw[:n-1]
		}
		return string(raw), nil
	}
	// UE4 negative length = UTF-16 code units including NUL.
	n := int(-n32)
	raw, err := r.raw(n * 2)
	if err != nil {
		return "", err
	}
	u := make([]uint16, 0, n)
	for i := 0; i+1 < len(raw); i += 2 {
		x := binary.LittleEndian.Uint16(raw[i : i+2])
		if x == 0 {
			break
		}
		u = append(u, x)
	}
	// Game property names/IDs are ASCII in observed saves. Preserve BMP text conservatively.
	var sb strings.Builder
	for _, x := range u {
		if x <= 0x7F {
			sb.WriteByte(byte(x))
		} else {
			sb.WriteRune(rune(x))
		}
	}
	return sb.String(), nil
}

type property struct {
	Name       string
	Type       string
	Size       int32
	ArrayIndex int32
	BoolTag    bool
	InnerType  string
	KeyType    string
	ValueType  string
	StructType string
	Value      any
}

type structValue struct{ Properties []property }
type arrayStructValue struct{ Items [][]property }
type mapEntry struct {
	Key   any
	Value any
}
type mapValue struct{ Items []mapEntry }

func parseTag(r *gvasReader) (property, int, error) {
	var p property
	start := r.o
	name, err := r.fstr()
	if err != nil {
		return p, 0, err
	}
	p.Name = name
	if name == "None" {
		return p, r.o, nil
	}
	typ, err := r.fstr()
	if err != nil {
		return p, 0, err
	}
	p.Type = typ
	sz, err := r.i32()
	if err != nil {
		return p, 0, err
	}
	ai, err := r.i32()
	if err != nil {
		return p, 0, err
	}
	if sz < 0 || sz > int32(len(r.b)) {
		return p, 0, fmt.Errorf("invalid property size %d for %s at 0x%X", sz, name, start)
	}
	p.Size, p.ArrayIndex = sz, ai

	switch typ {
	case "StructProperty":
		p.StructType, err = r.fstr()
		if err != nil {
			return p, 0, err
		}
		if _, err = r.raw(16); err != nil {
			return p, 0, err
		}
	case "BoolProperty":
		v, e := r.u8()
		if e != nil {
			return p, 0, e
		}
		p.BoolTag = v != 0
	case "ByteProperty", "EnumProperty":
		_, err = r.fstr()
		if err != nil {
			return p, 0, err
		}
	case "ArrayProperty", "SetProperty":
		p.InnerType, err = r.fstr()
		if err != nil {
			return p, 0, err
		}
	case "MapProperty":
		p.KeyType, err = r.fstr()
		if err != nil {
			return p, 0, err
		}
		p.ValueType, err = r.fstr()
		if err != nil {
			return p, 0, err
		}
	}
	hasGuid, err := r.u8()
	if err != nil {
		return p, 0, err
	}
	if hasGuid != 0 {
		if _, err = r.raw(16); err != nil {
			return p, 0, err
		}
	}
	return p, r.o + int(p.Size), nil
}

func parseProperties(r *gvasReader, end int) ([]property, error) {
	var out []property
	for end <= 0 || r.o < end {
		p, expectedEnd, err := parseTag(r)
		if err != nil {
			return nil, err
		}
		if p.Name == "None" {
			return out, nil
		}
		if expectedEnd < r.o || expectedEnd > len(r.b) {
			return nil, fmt.Errorf("invalid end for property %s", p.Name)
		}
		if err := parseValue(r, &p, expectedEnd); err != nil {
			return nil, fmt.Errorf("%s/%s: %w", p.Name, p.Type, err)
		}
		if p.Type != "BoolProperty" && r.o != expectedEnd {
			// Complex serialization can include data we intentionally did not model. Fail closed rather than drift.
			return nil, fmt.Errorf("property %s parsed to 0x%X, expected 0x%X", p.Name, r.o, expectedEnd)
		}
		out = append(out, p)
	}
	return out, nil
}

func parseSimple(r *gvasReader, typ string) (any, error) {
	switch typ {
	case "StrProperty", "NameProperty", "ObjectProperty":
		return r.fstr()
	case "IntProperty":
		v, e := r.i32()
		return int(v), e
	case "FloatProperty":
		v, e := r.f32()
		return float64(v), e
	case "ByteProperty":
		v, e := r.u8()
		return int(v), e
	case "BoolProperty":
		v, e := r.u8()
		return v != 0, e
	default:
		return nil, fmt.Errorf("unsupported simple map type %s", typ)
	}
}

func parseValue(r *gvasReader, p *property, expectedEnd int) error {
	switch p.Type {
	case "IntProperty":
		v, err := r.i32()
		if err != nil {
			return err
		}
		p.Value = int(v)
	case "FloatProperty":
		v, err := r.f32()
		if err != nil {
			return err
		}
		p.Value = float64(v)
	case "BoolProperty":
		p.Value = p.BoolTag
	case "StrProperty", "NameProperty", "ObjectProperty", "EnumProperty":
		v, err := r.fstr()
		if err != nil {
			return err
		}
		p.Value = v
	case "ByteProperty":
		if p.Size == 1 {
			v, err := r.u8()
			if err != nil {
				return err
			}
			p.Value = int(v)
		} else {
			raw, err := r.raw(int(p.Size))
			if err != nil {
				return err
			}
			p.Value = append([]byte(nil), raw...)
		}
	case "TextProperty":
		raw, err := r.raw(int(p.Size))
		if err != nil {
			return err
		}
		p.Value = append([]byte(nil), raw...)
	case "StructProperty":
		// Relevant game structs are property streams. DateTime is an 8-byte native struct.
		if p.StructType == "DateTime" || p.StructType == "Guid" || p.StructType == "Vector" || p.StructType == "Rotator" || p.StructType == "Transform" || p.StructType == "LinearColor" {
			raw, err := r.raw(int(p.Size))
			if err != nil {
				return err
			}
			p.Value = append([]byte(nil), raw...)
			return nil
		}
		props, err := parseProperties(r, expectedEnd)
		if err != nil {
			return err
		}
		p.Value = structValue{Properties: props}
	case "ArrayProperty":
		count32, err := r.i32()
		if err != nil {
			return err
		}
		if count32 < 0 || count32 > 100000 {
			return fmt.Errorf("invalid array count %d", count32)
		}
		count := int(count32)
		switch p.InnerType {
		case "StrProperty":
			arr := make([]string, 0, count)
			for i := 0; i < count; i++ {
				s, e := r.fstr()
				if e != nil {
					return e
				}
				arr = append(arr, s)
			}
			p.Value = arr
		case "ByteProperty":
			raw, e := r.raw(count)
			if e != nil {
				return e
			}
			arr := make([]int, count)
			for i, b := range raw {
				arr[i] = int(b)
			}
			p.Value = arr
		case "IntProperty":
			arr := make([]int, 0, count)
			for i := 0; i < count; i++ {
				v, e := r.i32()
				if e != nil {
					return e
				}
				arr = append(arr, int(v))
			}
			p.Value = arr
		case "StructProperty":
			// UE4 writes an inner StructProperty descriptor before the struct elements.
			_, innerExpected, e := parseTag(r)
			if e != nil {
				return e
			}
			if innerExpected != expectedEnd {
				return fmt.Errorf("inner struct span mismatch 0x%X != 0x%X", innerExpected, expectedEnd)
			}
			items := make([][]property, 0, count)
			for i := 0; i < count; i++ {
				it, e := parseProperties(r, 0)
				if e != nil {
					return e
				}
				items = append(items, it)
			}
			p.Value = arrayStructValue{Items: items}
		default:
			return fmt.Errorf("unsupported array inner type %s", p.InnerType)
		}
	case "MapProperty":
		removed32, err := r.i32()
		if err != nil {
			return err
		}
		if removed32 < 0 || removed32 > 100000 {
			return fmt.Errorf("invalid removed map count %d", removed32)
		}
		for i := 0; i < int(removed32); i++ {
			if _, e := parseSimple(r, p.KeyType); e != nil {
				return e
			}
		}
		count32, err := r.i32()
		if err != nil {
			return err
		}
		if count32 < 0 || count32 > 100000 {
			return fmt.Errorf("invalid map count %d", count32)
		}
		mv := mapValue{}
		for i := 0; i < int(count32); i++ {
			k, e := parseSimple(r, p.KeyType)
			if e != nil {
				return e
			}
			var v any
			if p.ValueType == "StructProperty" {
				props, e2 := parseProperties(r, 0)
				if e2 != nil {
					return e2
				}
				v = []property(props)
			} else {
				v, e = parseSimple(r, p.ValueType)
				if e != nil {
					return e
				}
			}
			mv.Items = append(mv.Items, mapEntry{Key: k, Value: v})
		}
		p.Value = mv
	default:
		// Unsupported properties are safe to skip, provided the serialized size is bounded.
		raw, err := r.raw(int(p.Size))
		if err != nil {
			return err
		}
		p.Value = append([]byte(nil), raw...)
	}
	return nil
}

func parseFlowSave(path string) ([][]property, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	if len(b) < 16 || string(b[:4]) != "GVAS" {
		return nil, "", errors.New("not an Unreal Engine GVAS save")
	}
	sum := sha256.Sum256(b)
	hash := hex.EncodeToString(sum[:])
	classBytes := []byte("/Script/SM2.SM2SaveGameFlow\x00")
	pos := bytes.Index(b, classBytes)
	if pos < 4 {
		return nil, hash, errors.New("SM2 save class not found")
	}
	r := &gvasReader{b: b, o: pos - 4}
	cls, err := r.fstr()
	if err != nil {
		return nil, hash, err
	}
	if cls != "/Script/SM2.SM2SaveGameFlow" {
		return nil, hash, fmt.Errorf("unexpected save class %q", cls)
	}
	props, err := parseProperties(r, 0)
	if err != nil {
		return nil, hash, err
	}
	fp := findProp(props, "FlowSaveSlots")
	if fp == nil {
		return nil, hash, errors.New("FlowSaveSlots not found")
	}
	arr, ok := fp.Value.(arrayStructValue)
	if !ok {
		return nil, hash, errors.New("FlowSaveSlots has unexpected format")
	}
	if len(arr.Items) == 0 || len(arr.Items) > 16 {
		return nil, hash, fmt.Errorf("unexpected save slot count %d", len(arr.Items))
	}
	return arr.Items, hash, nil
}

func findProp(props []property, name string) *property {
	for i := range props {
		if props[i].Name == name {
			return &props[i]
		}
	}
	return nil
}
func boolProp(props []property, name string) (bool, bool) {
	p := findProp(props, name)
	if p == nil {
		return false, false
	}
	v, ok := p.Value.(bool)
	return v, ok
}
func intProp(props []property, name string) (int, bool) {
	p := findProp(props, name)
	if p == nil {
		return 0, false
	}
	v, ok := p.Value.(int)
	return v, ok
}
func floatProp(props []property, name string) (float64, bool) {
	p := findProp(props, name)
	if p == nil {
		return 0, false
	}
	v, ok := p.Value.(float64)
	return v, ok
}
func strProp(props []property, name string) (string, bool) {
	p := findProp(props, name)
	if p == nil {
		return "", false
	}
	v, ok := p.Value.(string)
	return v, ok
}
func intArrayProp(props []property, name string) ([]int, bool) {
	p := findProp(props, name)
	if p == nil {
		return nil, false
	}
	v, ok := p.Value.([]int)
	return v, ok
}

// ---------------- Save model ----------------

type challengeData struct {
	Difficulty int
	Unlocked   bool
	Biome      int
	LevelID    string
}
type contaminationData struct{ Current, Global float64 }
type upgradeData struct {
	Weapon []int
	Stats  []int
}
type slotData struct {
	Number            int
	Used              bool
	Time              float64
	CurrentDifficulty string
	Levels            map[string]bool
	Challenges        map[string]challengeData
	Contamination     map[string]contaminationData
	Upgrades          []upgradeData
	EquippedCosmetic  bool
	CumulatedCurrency float64
	EnemiesKilled     int
}

func modelSlots(raw [][]property) ([]slotData, error) {
	var out []slotData
	for idx, props := range raw {
		s := slotData{Number: idx + 1, Levels: map[string]bool{}, Challenges: map[string]challengeData{}, Contamination: map[string]contaminationData{}}
		s.Used, _ = boolProp(props, "bIsSlotUsed")
		s.Time, _ = floatProp(props, "Time")
		s.CurrentDifficulty, _ = strProp(props, "CurrentDifficulty")
		if p := findProp(props, "LevelSaves"); p != nil {
			mv, ok := p.Value.(mapValue)
			if !ok {
				return nil, fmt.Errorf("slot %d LevelSaves format", idx+1)
			}
			for _, e := range mv.Items {
				key, ok := e.Key.(string)
				if !ok {
					continue
				}
				vp, ok := e.Value.([]property)
				if !ok {
					continue
				}
				fin, _ := boolProp(vp, "bIsLevelFinished")
				s.Levels[key] = fin
			}
		}
		if p := findProp(props, "ChallengeSaves"); p != nil {
			av, ok := p.Value.(arrayStructValue)
			if !ok {
				return nil, fmt.Errorf("slot %d ChallengeSaves format", idx+1)
			}
			for _, it := range av.Items {
				id, _ := strProp(it, "ChallengeID")
				if id == "" {
					continue
				}
				d, _ := intProp(it, "CurrentDifficulty")
				u, _ := boolProp(it, "bIsUnlocked")
				b, _ := intProp(it, "BiomeType")
				lv, _ := strProp(it, "LevelID")
				s.Challenges[id] = challengeData{d, u, b, lv}
			}
		}
		if p := findProp(props, "ContaminationLevelSave"); p != nil {
			mv, ok := p.Value.(mapValue)
			if !ok {
				return nil, fmt.Errorf("slot %d contamination format", idx+1)
			}
			for _, e := range mv.Items {
				key, ok := e.Key.(string)
				if !ok {
					continue
				}
				vp, ok := e.Value.([]property)
				if !ok {
					continue
				}
				cur, _ := floatProp(vp, "CurrentWeight")
				glob, _ := floatProp(vp, "GlobalWeight")
				s.Contamination[key] = contaminationData{cur, glob}
			}
		}
		if p := findProp(props, "PlayerSave"); p != nil {
			sv, ok := p.Value.(structValue)
			if !ok {
				return nil, fmt.Errorf("slot %d PlayerSave format", idx+1)
			}
			pp := sv.Properties
			if up := findProp(pp, "UpgradeSaves"); up != nil {
				av, ok := up.Value.(arrayStructValue)
				if ok {
					for _, it := range av.Items {
						w, _ := intArrayProp(it, "WeaponsUpgradeLevel")
						st, _ := intArrayProp(it, "StatsUpgradeLevel")
						s.Upgrades = append(s.Upgrades, upgradeData{w, st})
					}
				}
			}
			s.CumulatedCurrency, _ = floatProp(pp, "CumulatedSoftCurrency")
			s.EnemiesKilled, _ = intProp(pp, "CristoEnemiesKilled")
			// Safe cosmetic proof: equipped IDs exist in PlayerSave.SkinSaves and are also in the slot's unlocked SkinSave list.
			unlocked := map[string]bool{}
			if sk := findProp(props, "SkinSave"); sk != nil {
				if arr, ok := sk.Value.([]string); ok {
					for _, x := range arr {
						unlocked[x] = true
					}
				}
			}
			if ss := findProp(pp, "SkinSaves"); ss != nil {
				if mv, ok := ss.Value.(mapValue); ok {
					for _, e := range mv.Items {
						vp, ok := e.Value.([]property)
						if !ok {
							continue
						}
						for _, nm := range []string{"HeadID", "BodyID", "SmurfizerID"} {
							id, _ := strProp(vp, nm)
							if id != "" && unlocked[id] {
								s.EquippedCosmetic = true
							}
						}
					}
				}
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// ---------------- Save difficulty helper ----------------

type gameDifficultyChoice struct {
	MenuName string
	EnumName string
	Help     string
}

var supportedGameDifficulties = []gameDifficultyChoice{
	{
		MenuName: "Story",
		EnumName: "EGameDifficulty::EASY",
		Help:     "easiest public difficulty",
	},
	{
		MenuName: "Epic",
		EnumName: "EGameDifficulty::MEDIUM",
		Help:     "normal public difficulty",
	},
	{
		MenuName: "Challenge",
		EnumName: "EGameDifficulty::HARD",
		Help:     "hard public difficulty; required for 'And his name is Zosimos...'",
	},
}

func difficultyByNumber(n int) (gameDifficultyChoice, bool) {
	if n < 1 || n > len(supportedGameDifficulties) {
		return gameDifficultyChoice{}, false
	}
	return supportedGameDifficulties[n-1], true
}

func difficultyByName(s string) (gameDifficultyChoice, bool) {
	n := strings.ToLower(strings.TrimSpace(s))
	for _, d := range supportedGameDifficulties {
		if strings.ToLower(d.MenuName) == n || strings.ToLower(d.EnumName) == n {
			return d, true
		}
	}
	return gameDifficultyChoice{}, false
}

func friendlyDifficulty(enumName string) string {
	for _, d := range supportedGameDifficulties {
		if d.EnumName == enumName {
			return fmt.Sprintf("%s (%s)", d.MenuName, d.EnumName)
		}
	}
	if enumName == "EGameDifficulty::HARD_PLUS" {
		return "HARD_PLUS (internal/undocumented; not exposed by this tool)"
	}
	if enumName == "" {
		return "(unknown)"
	}
	return enumName
}

type saveTagMeta struct {
	Name       string
	Type       string
	Size       int32
	SizePos    int
	ValueStart int
	ValueEnd   int
	BoolTag    bool
}

type difficultySlotPatch struct {
	Slot       int
	Used       bool
	Difficulty string
	SizePos    int
	ValueStart int
	ValueEnd   int
}

type difficultyLayout struct {
	RootSizePos  int
	InnerSizePos int
	Slots        []difficultySlotPatch
}

func readTagMeta(r *gvasReader) (saveTagMeta, bool, error) {
	var m saveTagMeta
	name, err := r.fstr()
	if err != nil {
		return m, false, err
	}
	m.Name = name
	if name == "None" {
		return m, true, nil
	}
	typ, err := r.fstr()
	if err != nil {
		return m, false, err
	}
	m.Type = typ
	m.SizePos = r.o
	sz, err := r.i32()
	if err != nil {
		return m, false, err
	}
	if sz < 0 || sz > int32(len(r.b)) {
		return m, false, fmt.Errorf("invalid property size %d for %s", sz, name)
	}
	m.Size = sz
	if _, err = r.i32(); err != nil {
		return m, false, err
	}

	switch typ {
	case "StructProperty":
		if _, err = r.fstr(); err != nil {
			return m, false, err
		}
		if _, err = r.raw(16); err != nil {
			return m, false, err
		}
	case "BoolProperty":
		v, e := r.u8()
		if e != nil {
			return m, false, e
		}
		m.BoolTag = v != 0
	case "ByteProperty", "EnumProperty":
		if _, err = r.fstr(); err != nil {
			return m, false, err
		}
	case "ArrayProperty", "SetProperty":
		if _, err = r.fstr(); err != nil {
			return m, false, err
		}
	case "MapProperty":
		if _, err = r.fstr(); err != nil {
			return m, false, err
		}
		if _, err = r.fstr(); err != nil {
			return m, false, err
		}
	}
	hasGuid, err := r.u8()
	if err != nil {
		return m, false, err
	}
	if hasGuid != 0 {
		if _, err = r.raw(16); err != nil {
			return m, false, err
		}
	}
	m.ValueStart = r.o
	m.ValueEnd = r.o + int(m.Size)
	if m.ValueEnd < m.ValueStart || m.ValueEnd > len(r.b) {
		return m, false, fmt.Errorf("invalid value span for %s", name)
	}
	return m, false, nil
}

func locateDifficultyLayout(b []byte) (difficultyLayout, error) {
	var out difficultyLayout
	if len(b) < 16 || string(b[:4]) != "GVAS" {
		return out, errors.New("not an Unreal Engine GVAS save")
	}
	classBytes := []byte("/Script/SM2.SM2SaveGameFlow\x00")
	pos := bytes.Index(b, classBytes)
	if pos < 4 {
		return out, errors.New("SM2 save class not found")
	}
	r := &gvasReader{b: b, o: pos - 4}
	cls, err := r.fstr()
	if err != nil {
		return out, err
	}
	if cls != "/Script/SM2.SM2SaveGameFlow" {
		return out, fmt.Errorf("unexpected save class %q", cls)
	}

	var root saveTagMeta
	for {
		m, finished, e := readTagMeta(r)
		if e != nil {
			return out, e
		}
		if finished {
			return out, errors.New("FlowSaveSlots not found")
		}
		if m.Name == "FlowSaveSlots" {
			root = m
			break
		}
		r.o = m.ValueEnd
	}
	if root.Type != "ArrayProperty" {
		return out, fmt.Errorf("FlowSaveSlots is %s, expected ArrayProperty", root.Type)
	}
	out.RootSizePos = root.SizePos

	r.o = root.ValueStart
	count32, err := r.i32()
	if err != nil {
		return out, err
	}
	if count32 <= 0 || count32 > 16 {
		return out, fmt.Errorf("unexpected save slot count %d", count32)
	}

	inner, finished, err := readTagMeta(r)
	if err != nil {
		return out, err
	}
	if finished || inner.Type != "StructProperty" {
		return out, errors.New("FlowSaveSlots inner struct descriptor missing")
	}
	if inner.ValueEnd != root.ValueEnd {
		return out, fmt.Errorf("FlowSaveSlots inner span mismatch 0x%X != 0x%X", inner.ValueEnd, root.ValueEnd)
	}
	out.InnerSizePos = inner.SizePos

	for slot := 1; slot <= int(count32); slot++ {
		d := difficultySlotPatch{Slot: slot}
		foundDifficulty := false
		for {
			m, itemEnd, e := readTagMeta(r)
			if e != nil {
				return out, fmt.Errorf("slot %d: %w", slot, e)
			}
			if itemEnd {
				break
			}
			if m.Name == "bIsSlotUsed" && m.Type == "BoolProperty" {
				d.Used = m.BoolTag
			}
			if m.Name == "CurrentDifficulty" && m.Type == "EnumProperty" {
				vr := &gvasReader{b: b, o: m.ValueStart}
				v, e := vr.fstr()
				if e != nil || vr.o != m.ValueEnd {
					return out, fmt.Errorf("slot %d CurrentDifficulty has unexpected encoding", slot)
				}
				d.Difficulty = v
				d.SizePos = m.SizePos
				d.ValueStart = m.ValueStart
				d.ValueEnd = m.ValueEnd
				foundDifficulty = true
			}
			r.o = m.ValueEnd
		}
		if !foundDifficulty {
			return out, fmt.Errorf("slot %d CurrentDifficulty not found", slot)
		}
		out.Slots = append(out.Slots, d)
	}
	if r.o != root.ValueEnd {
		return out, fmt.Errorf("slot stream ended at 0x%X, expected 0x%X", r.o, root.ValueEnd)
	}
	return out, nil
}

func encodeAnsiFString(s string) []byte {
	v := append([]byte(s), 0)
	out := make([]byte, 4+len(v))
	binary.LittleEndian.PutUint32(out[:4], uint32(len(v)))
	copy(out[4:], v)
	return out
}

func patchDifficultyBytes(b []byte, slotNumber int, target gameDifficultyChoice) ([]byte, string, error) {
	layout, err := locateDifficultyLayout(b)
	if err != nil {
		return nil, "", err
	}
	if slotNumber < 1 || slotNumber > len(layout.Slots) {
		return nil, "", fmt.Errorf("slot %d does not exist", slotNumber)
	}
	slot := layout.Slots[slotNumber-1]
	if !slot.Used {
		return nil, "", fmt.Errorf("slot %d is unused; refusing to modify it", slotNumber)
	}
	if slot.Difficulty == target.EnumName {
		return append([]byte(nil), b...), slot.Difficulty, nil
	}
	if !strings.HasPrefix(slot.Difficulty, "EGameDifficulty::") {
		return nil, "", fmt.Errorf("slot %d has unexpected difficulty %q", slotNumber, slot.Difficulty)
	}

	newValue := encodeAnsiFString(target.EnumName)
	oldSerialized := slot.ValueEnd - slot.ValueStart
	delta := len(newValue) - oldSerialized
	patched := append([]byte(nil), b...)

	rootSize := int32(binary.LittleEndian.Uint32(patched[layout.RootSizePos : layout.RootSizePos+4]))
	innerSize := int32(binary.LittleEndian.Uint32(patched[layout.InnerSizePos : layout.InnerSizePos+4]))
	propSize := int32(binary.LittleEndian.Uint32(patched[slot.SizePos : slot.SizePos+4]))
	if rootSize+int32(delta) <= 0 || innerSize+int32(delta) <= 0 || propSize+int32(delta) <= 0 {
		return nil, "", errors.New("difficulty patch would produce invalid property sizes")
	}
	binary.LittleEndian.PutUint32(patched[layout.RootSizePos:layout.RootSizePos+4], uint32(rootSize+int32(delta)))
	binary.LittleEndian.PutUint32(patched[layout.InnerSizePos:layout.InnerSizePos+4], uint32(innerSize+int32(delta)))
	binary.LittleEndian.PutUint32(patched[slot.SizePos:slot.SizePos+4], uint32(propSize+int32(delta)))

	out := make([]byte, 0, len(patched)+delta)
	out = append(out, patched[:slot.ValueStart]...)
	out = append(out, newValue...)
	out = append(out, patched[slot.ValueEnd:]...)
	return out, slot.Difficulty, nil
}

func uniqueBackupPath(savePath string) string {
	base := savePath + ".Backup"
	if _, err := os.Stat(base); os.IsNotExist(err) {
		return base
	}
	stamp := time.Now().Format("20060102_150405")
	return savePath + ".Backup_" + stamp
}

func setGameDifficulty(savePath string, slotNumber int, target gameDifficultyChoice) error {
	original, err := os.ReadFile(savePath)
	if err != nil {
		return err
	}
	beforeSlots, beforeHash, err := parseFlowSave(savePath)
	if err != nil {
		return fmt.Errorf("pre-patch save validation failed: %w", err)
	}
	beforeModel, err := modelSlots(beforeSlots)
	if err != nil {
		return err
	}
	if slotNumber < 1 || slotNumber > len(beforeModel) || !beforeModel[slotNumber-1].Used {
		return fmt.Errorf("slot %d is not a used save slot", slotNumber)
	}

	patched, oldDifficulty, err := patchDifficultyBytes(original, slotNumber, target)
	if err != nil {
		return err
	}
	if oldDifficulty == target.EnumName {
		fmt.Printf("[OK] Slot %d is already set to %s.\n", slotNumber, friendlyDifficulty(target.EnumName))
		return nil
	}

	tmp := savePath + ".difficulty.tmp"
	if err := os.WriteFile(tmp, patched, 0644); err != nil {
		return err
	}
	defer os.Remove(tmp)

	checkSlots, _, err := parseFlowSave(tmp)
	if err != nil {
		return fmt.Errorf("patched save failed structural validation: %w", err)
	}
	checkModel, err := modelSlots(checkSlots)
	if err != nil {
		return fmt.Errorf("patched save failed model validation: %w", err)
	}
	if slotNumber > len(checkModel) || checkModel[slotNumber-1].CurrentDifficulty != target.EnumName {
		return fmt.Errorf("patched save did not verify as %s", target.EnumName)
	}

	backup := uniqueBackupPath(savePath)
	if err := os.WriteFile(backup, original, 0644); err != nil {
		return fmt.Errorf("backup creation failed: %w", err)
	}
	if err := os.WriteFile(savePath, patched, 0644); err != nil {
		_ = os.WriteFile(savePath, original, 0644)
		return fmt.Errorf("save write failed; original was restored: %w", err)
	}

	afterSlots, afterHash, err := parseFlowSave(savePath)
	if err != nil {
		_ = os.WriteFile(savePath, original, 0644)
		return fmt.Errorf("post-write validation failed; original was restored: %w", err)
	}
	afterModel, err := modelSlots(afterSlots)
	if err != nil || slotNumber > len(afterModel) || afterModel[slotNumber-1].CurrentDifficulty != target.EnumName {
		_ = os.WriteFile(savePath, original, 0644)
		return errors.New("post-write difficulty verification failed; original was restored")
	}

	fmt.Printf("[OK] Backup: %s\n", backup)
	fmt.Printf("[OK] Slot %d difficulty: %s -> %s\n",
		slotNumber, friendlyDifficulty(oldDifficulty), friendlyDifficulty(target.EnumName))
	fmt.Printf("[OK] Save SHA-256: %s -> %s\n", beforeHash, afterHash)
	fmt.Println("[OK] Patched save reparsed successfully after writing.")
	if target.MenuName == "Challenge" {
		fmt.Println("[INFO] Challenge is the difficulty required for 'And his name is Zosimos...'.")
		fmt.Println("[INFO] For the Stolas achievement, use Challenge for the STORY encounter with Stolas.")
		fmt.Println("[INFO] Later optional replay/challenge versions of the boss may not trigger the achievement reliably.")
		fmt.Println("[INFO] After changing difficulty, load the save once. If a level hangs, restart the game and load it again.")
		fmt.Println("[INFO] After the fight, you can run this helper again and switch the slot back to Story or Epic.")
	}
	return nil
}

func difficultyHelper(savePath string) error {
	raw, _, err := parseFlowSave(savePath)
	if err != nil {
		return err
	}
	slots, err := modelSlots(raw)
	if err != nil {
		return err
	}

	fmt.Println("\n====================== SAVE DIFFICULTY HELPER ======================")
	fmt.Println("This OPTIONAL feature modifies flow.sav. A full backup is created first.")
	fmt.Println("It changes only CurrentDifficulty in the selected USED save slot.")
	fmt.Println()
	fmt.Println("Public game difficulties:")
	fmt.Println("  1) Story     = EGameDifficulty::EASY")
	fmt.Println("  2) Epic      = EGameDifficulty::MEDIUM")
	fmt.Println("  3) Challenge = EGameDifficulty::HARD")
	fmt.Println()
	fmt.Println("Achievement note:")
	fmt.Println("  'And his name is Zosimos...' requires defeating Stolas on Challenge.")
	fmt.Println("  Use Challenge for the STORY encounter with Stolas; later optional replays may not trigger it reliably.")
	fmt.Println("  After editing difficulty, load the save once; if a level hangs, restart the game and load it again.")
	fmt.Println("  After defeating Stolas, the difficulty can be changed back at any time.")
	fmt.Println("  EGameDifficulty::HARD_PLUS exists internally but is undocumented and is NOT exposed.")
	fmt.Println("-------------------------------------------------------------------------")

	for _, s := range slots {
		state := "unused"
		if s.Used {
			state = friendlyDifficulty(s.CurrentDifficulty)
		}
		fmt.Printf("  Slot %d: %s\n", s.Number, state)
	}

	fmt.Print("Select a USED slot number, or press Enter to cancel: ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		fmt.Println("Difficulty helper cancelled.")
		return nil
	}
	slotNumber, err := strconv.Atoi(line)
	if err != nil || slotNumber < 1 || slotNumber > len(slots) || !slots[slotNumber-1].Used {
		return errors.New("invalid or unused slot selection")
	}

	fmt.Println()
	for i, d := range supportedGameDifficulties {
		suffix := ""
		if d.MenuName == "Challenge" {
			suffix = "  <-- required for the Stolas difficulty achievement"
		}
		fmt.Printf("  %d) %-9s %s%s\n", i+1, d.MenuName, d.Help, suffix)
	}
	fmt.Print("Choose the new difficulty, or press Enter to cancel: ")
	diffLine, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	diffLine = strings.TrimSpace(diffLine)
	if diffLine == "" {
		fmt.Println("Difficulty helper cancelled.")
		return nil
	}
	diffNumber, err := strconv.Atoi(diffLine)
	if err != nil {
		return errors.New("invalid difficulty selection")
	}
	target, ok := difficultyByNumber(diffNumber)
	if !ok {
		return errors.New("invalid difficulty selection")
	}

	fmt.Printf("Slot %d will be changed to %s. Type APPLY to continue: ", slotNumber, friendlyDifficulty(target.EnumName))
	confirm, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.TrimSpace(confirm) != "APPLY" {
		fmt.Println("Difficulty helper cancelled. Nothing was changed.")
		return nil
	}
	return setGameDifficulty(savePath, slotNumber, target)
}

// ---------------- Achievement rules ----------------

type evalStatus int

const (
	statusNotMet evalStatus = iota
	statusVerified
	statusInfo
)

type evalResult struct {
	Status   evalStatus
	Evidence string
}
type rule struct {
	Key, Name, Description string
	Manual                 bool
	Eval                   func([]slotData) evalResult
}

func usedSlots(slots []slotData) []slotData {
	var u []slotData
	for _, s := range slots {
		if s.Used {
			u = append(u, s)
		}
	}
	return u
}
func anySlot(slots []slotData, f func(slotData) (bool, string)) (bool, string) {
	for _, s := range usedSlots(slots) {
		if ok, ev := f(s); ok {
			return true, fmt.Sprintf("Slot %d: %s", s.Number, ev)
		}
	}
	return false, ""
}
func allFinished(s slotData, levels []string) bool {
	for _, lv := range levels {
		if !s.Levels[lv] {
			return false
		}
	}
	return true
}
func allChallenges(s slotData, ids []string, minDifficulty int, needUnlocked bool) bool {
	for _, id := range ids {
		c, ok := s.Challenges[id]
		if !ok {
			return false
		}
		if needUnlocked && !c.Unlocked {
			return false
		}
		if c.Difficulty < minDifficulty {
			return false
		}
	}
	return true
}
func anyChallenge(s slotData, minDifficulty int) bool {
	for _, c := range s.Challenges {
		if c.Unlocked && c.Difficulty >= minDifficulty {
			return true
		}
	}
	return false
}
func cleanLevels(s slotData, levels []string) (bool, string) {
	parts := []string{}
	complete := true
	for _, lv := range levels {
		c, ok := s.Contamination[lv]
		if !ok || c.Global <= 0 {
			parts = append(parts, "missing")
			complete = false
			continue
		}
		parts = append(parts, fmt.Sprintf("%.0f/%.0f", c.Current, c.Global))
		if c.Current+0.001 < c.Global {
			complete = false
		}
	}
	return complete, strings.Join(parts, ", ")
}
func maxStats(u upgradeData) bool {
	target := []int{4, 7, 7, 4, 4, 3}
	if len(u.Stats) < len(target) {
		return false
	}
	for i, v := range target {
		if u.Stats[i] < v {
			return false
		}
	}
	return true
}
func maxWeapon(u upgradeData) bool {
	if len(u.Weapon) < 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		if u.Weapon[i] < 2 {
			return false
		}
	}
	return true
}
func anyUpgrade(s slotData) bool {
	for _, u := range s.Upgrades {
		for _, v := range u.Weapon {
			if v > 0 {
				return true
			}
		}
		for _, v := range u.Stats {
			if v > 0 {
				return true
			}
		}
	}
	return false
}
func maxProgress(slots []slotData, f func(slotData) (float64, float64)) string {
	bestA, bestB := float64(0), float64(0)
	for _, s := range usedSlots(slots) {
		a, b := f(s)
		if b > 0 && a/b > func() float64 {
			if bestB == 0 {
				return -1
			}
			return bestA / bestB
		}() {
			bestA, bestB = a, b
		}
	}
	if bestB > 0 {
		return fmt.Sprintf("best save %.0f/%.0f", bestA, bestB)
	}
	return "condition not yet proven by any used slot"
}

func rules() []rule {
	F := []string{"L_01_Forest_Master", "L_02_Forest_Master", "L_03_Forest_Master"}
	M := []string{"L_04_Mountain_Master", "L_05_Mountain_Master", "L_06_Mountain_Master"}
	V := []string{"L_07_Volcano_Master", "L_08_Volcano_Master", "L_09_Volcano_Master"}
	D1 := []string{"D01", "D02", "D03", "D04", "D05", "D06"}
	D2 := []string{"D07", "D08", "D09", "D10", "D11", "D12"}
	D3 := []string{"D13", "D14", "D15", "D16", "D17", "D18"}
	DAll := append(append(append([]string{}, D1...), D2...), D3...)
	auto := func(key, name, desc string, fn func([]slotData) evalResult) rule {
		return rule{Key: key, Name: name, Description: desc, Eval: fn}
	}
	manual := func(key, name, desc string) rule {
		return rule{Key: key, Name: name, Description: desc, Manual: true, Eval: func([]slotData) evalResult {
			return evalResult{statusInfo, "Event-specific achievement: flow.sav contains no reliable persistent proof."}
		}}
	}
	chapter := func(key, name, desc string, ls []string) rule {
		return auto(key, name, desc, func(slots []slotData) evalResult {
			if ok, ev := anySlot(slots, func(s slotData) (bool, string) { return allFinished(s, ls), "all chapter levels are marked finished" }); ok {
				return evalResult{statusVerified, ev}
			}
			return evalResult{statusNotMet, "chapter completion is not fully recorded in any used slot"}
		})
	}
	boss := func(key, name, desc, lv string) rule {
		return auto(key, name, desc, func(slots []slotData) evalResult {
			if ok, ev := anySlot(slots, func(s slotData) (bool, string) { return s.Levels[lv], lv + " is marked finished" }); ok {
				return evalResult{statusVerified, ev}
			}
			return evalResult{statusNotMet, lv + " is not marked finished in any used slot"}
		})
	}
	clean := func(key, name, desc string, ls []string) rule {
		return auto(key, name, desc, func(slots []slotData) evalResult {
			best := ""
			for _, s := range usedSlots(slots) {
				ok, ev := cleanLevels(s, ls)
				if ok {
					return evalResult{statusVerified, fmt.Sprintf("Slot %d: %s", s.Number, ev)}
				}
				if ev != "" {
					best = fmt.Sprintf("Slot %d: %s", s.Number, ev)
				}
			}
			if best == "" {
				best = "no complete contamination record in used saves"
			}
			return evalResult{statusNotMet, best}
		})
	}
	challAll := func(key, name, desc string, ids []string, min int) rule {
		return auto(key, name, desc, func(slots []slotData) evalResult {
			if ok, ev := anySlot(slots, func(s slotData) (bool, string) {
				return allChallenges(s, ids, min, true), fmt.Sprintf("all %d required challenges are unlocked at rank >= %d", len(ids), min)
			}); ok {
				return evalResult{statusVerified, ev}
			}
			return evalResult{statusNotMet, "required challenge ranks are not complete in any used slot"}
		})
	}
	upgradeChar := func(key, name, desc string, idx int) rule {
		return auto(key, name, desc, func(slots []slotData) evalResult {
			if ok, ev := anySlot(slots, func(s slotData) (bool, string) {
				if len(s.Upgrades) <= idx {
					return false, ""
				}
				return maxStats(s.Upgrades[idx]), "all six character Crystal stat thresholds are reached"
			}); ok {
				return evalResult{statusVerified, ev}
			}
			return evalResult{statusNotMet, "Crystal stat thresholds are not all reached"}
		})
	}
	upgradeSub := func(key, name, desc string, idx int) rule {
		return auto(key, name, desc, func(slots []slotData) evalResult {
			if ok, ev := anySlot(slots, func(s slotData) (bool, string) {
				if len(s.Upgrades) <= idx {
					return false, ""
				}
				return maxWeapon(s.Upgrades[idx]), "all three substance Crystal upgrade levels are >= 2"
			}); ok {
				return evalResult{statusVerified, ev}
			}
			return evalResult{statusNotMet, "Crystal substance thresholds are not all reached"}
		})
	}

	return []rule{
		chapter("CHAP_FOREST_FINISH", "Let's take a walk in the woods...", "Complete the Smurf Forest levels", F),
		chapter("CHAP_MOUNTAIN_FINISH", "Conquer the mountain!", "Complete the Snowy Mountains levels", M),
		chapter("CHAP_VOLCANO_FINISH", "Flee, poor Smurfs!", "Complete The Land of Fire levels", V),
		boss("BOSS_MOUNTAIN", "Double-dealing", "Beat the double created by Stolas", "L_06_Mountain_Master"),
		boss("BOSS_FOREST", "Slimy kisses", "Beat the CrystoToad", "L_03_Forest_Master"),
		boss("BOSS_VOID", "Prison No Break", "Beat Stolas", "L_10_Void_Master"),
		manual("BOSS_VOID_HARD", "And his name is Zosimos...", "Beat Stolas on Challenge difficulty"),
		clean("CLEAN_FOREST", "Spring cleaning", "Completely decrystallize the Smurf Forest levels", F),
		clean("CLEAN_MOUNTAIN", "Crystalline frost", "Completely decrystallize the Snowy Mountains levels", M),
		clean("CLEAN_VOLCANO", "The floor is lava", "Completely decrystallize The Land of Fire levels", V),
		auto("FIND_ALL_CHALLENGES", "A study in scarlet", "Find all the replayable challenges", func(slots []slotData) evalResult {
			if ok, ev := anySlot(slots, func(s slotData) (bool, string) {
				return allChallenges(s, DAll, 0, true), "all 18 challenges D01-D18 are unlocked"
			}); ok {
				return evalResult{statusVerified, ev}
			}
			return evalResult{statusNotMet, "not all 18 replayable challenges are unlocked"}
		}),
		auto("ANY_CHALLENGES_GOLD", "Becoming Midas", "Achieve a Gold difficulty for a replayable challenge", func(slots []slotData) evalResult {
			if ok, ev := anySlot(slots, func(s slotData) (bool, string) {
				return anyChallenge(s, 3), "at least one unlocked challenge has saved rank >= 3 (Gold)"
			}); ok {
				return evalResult{statusVerified, ev}
			}
			return evalResult{statusNotMet, "no saved Gold challenge rank found"}
		}),
		auto("ANY_CHALLENGES_CRYSTAL", "Hear, feel, think...", "Achieve a Crystal difficulty for a replayable challenge", func(slots []slotData) evalResult {
			if ok, ev := anySlot(slots, func(s slotData) (bool, string) {
				return anyChallenge(s, 4), "at least one unlocked challenge has saved rank >= 4 (Crystal)"
			}); ok {
				return evalResult{statusVerified, ev}
			}
			return evalResult{statusNotMet, "no saved Crystal challenge rank found"}
		}),
		challAll("CHALLENGES_GOLD_FOREST", "King of the Forest", "Complete all Smurf Forest challenges on Gold difficulty", D1, 3),
		challAll("CHALLENGES_GOLD_MOUNTAIN", "The Gold Mountain", "Complete all Snowy Mountain challenges on Gold difficulty", D2, 3),
		challAll("CHALLENGES_GOLD_VOLCANO", "Hot stuff", "Complete all The Land of Fire challenges on Gold difficulty", D3, 3),
		auto("UPGRADE_WEAPON", "Odds and ends", "Improve the SmurfoMix", func(slots []slotData) evalResult {
			if ok, ev := anySlot(slots, func(s slotData) (bool, string) {
				return anyUpgrade(s), "at least one saved SmurfoMix upgrade level is above 0"
			}); ok {
				return evalResult{statusVerified, ev}
			}
			return evalResult{statusNotMet, "no saved SmurfoMix upgrade found"}
		}),
		upgradeChar("UPGRADE_STORM_CRYSTAL", "Intrepid adventurer", "Obtain all of Storm's Crystal type improvements", 0),
		upgradeChar("UPGRADE_DIMWITTY_CRYSTAL", "Happy idiot", "Obtain all of Dimwitty's Crystal type improvements", 1),
		upgradeChar("UPGRADE_BRAINY_CRYSTAL", "Two-telligent", "Obtain all of Brainy Smurf's Crystal type improvements", 2),
		upgradeChar("UPGRADE_HANDY_CRYSTAL", "Trust me, I'm an engineer", "Obtain all of Handy Smurf's Crystal type improvements", 3),
		upgradeSub("UPGRADE_GREEN_STONE_CRYSTAL", "Get to the point", "Obtain all of the Green Stone's Crystal type improvements", 4),
		upgradeSub("UPGRADE_COLTOU_CRYSTAL", "StickAll everywhere", "Obtain all of the Crystal type improvements for the StickAll substances", 5),
		upgradeSub("UPGRADE_POUSSTOU_CRYSTAL", "Push me excuse you", "Obtain all of the Crystal type improvements for the PushAll substances", 6),
		upgradeSub("UPGRADE_CHOPTOU_CRYSTAL", "Catch as catch can!", "Obtain all of the Crystal type improvements for the CatchAll substances", 7),
		manual("ULTIMATE_STORM", "Eye of the Lynx", "Hit a weak spot from more than 80 m when playing Storm"),
		manual("ULTIMATE_DIMWITTY", "Carrot cake", "Gather at least 10 CrystoBeasts in the same place when playing Dimwitty"),
		manual("ULTIMATE_BRAINY", "The Apprentice Smurf", "Beat at least 10 CrystoBeasts with the alchemical vial when playing Brainy Smurf"),
		manual("ULTIMATE_HANDY", "Ex Machina", "Beat at least 30 CrystoBeasts during the TurboMix when playing Handy Smurf"),
		manual("CHALLENGE_COLTOU", "Come back to Earth", "Completely entangle a CrystoShift with the StickAll substance"),
		manual("CHALLENGE_POUSSTOU", "Talk to the hand", "Beat a CrystoSlap by shooting it into the void using the PushAll Substance"),
		manual("CHALLENGE_CHOPTOU", "Catch them all!", "Target 8 enemies with the CatchAll Substance"),
		manual("OVERHEAT_WEAPON", "Release the pressure", "Overheat the SmurfoMix once!"),
		auto("WEAR_SKIN", "Vanities", "Equip a cosmetic", func(slots []slotData) evalResult {
			if ok, ev := anySlot(slots, func(s slotData) (bool, string) {
				return s.EquippedCosmetic, "an equipped cosmetic ID matches the slot's unlocked cosmetic list"
			}); ok {
				return evalResult{statusVerified, ev}
			}
			return evalResult{statusNotMet, "no safely provable equipped cosmetic found"}
		}),
		auto("BEAT_ENNEMIES", "The lurking fear", "Beat 1000 CrystoBeasts", func(slots []slotData) evalResult {
			best := 0
			for _, s := range usedSlots(slots) {
				if s.EnemiesKilled > best {
					best = s.EnemiesKilled
				}
				if s.EnemiesKilled >= 1000 {
					return evalResult{statusVerified, fmt.Sprintf("Slot %d: %d / 1000 CrystoBeasts", s.Number, s.EnemiesKilled)}
				}
			}
			return evalResult{statusNotMet, fmt.Sprintf("best save: %d / 1000 CrystoBeasts", best)}
		}),
		auto("EARN_CURRENCY", "Numismatist", "Collect 1 million essence of nothingness", func(slots []slotData) evalResult {
			best := float64(0)
			for _, s := range usedSlots(slots) {
				if s.CumulatedCurrency > best {
					best = s.CumulatedCurrency
				}
				if s.CumulatedCurrency >= 1000000 {
					return evalResult{statusVerified, fmt.Sprintf("Slot %d: %.0f / 1000000 cumulative essence", s.Number, s.CumulatedCurrency)}
				}
			}
			return evalResult{statusNotMet, fmt.Sprintf("best save: %.0f / 1000000 cumulative essence", best)}
		}),
	}
}

// ---------------- GOG networking ----------------

func registryRefreshToken() (string, error) {
	cmd := exec.Command("reg", "query", `HKCU\Software\GOG.com\Galaxy`, "/v", "refreshToken")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("reg query failed: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(strings.ToLower(line), "refreshtoken") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			token := strings.TrimSpace(strings.Join(fields[2:], " "))
			if token != "" {
				return token, nil
			}
		}
	}
	return "", errors.New("refreshToken registry value is empty")
}

func fetchBuildDetails() (buildDetails, string, error) {
	var pd productData
	req, _ := http.NewRequest("GET", fmt.Sprintf("https://www.gogdb.org/data/products/%s.json", productID), nil)
	req.Header.Set("User-Agent", "Smurfs2-GOG-Achievement-Repair/1.1.0")
	resp, err := httpClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			_ = json.NewDecoder(resp.Body).Decode(&pd)
		}
	}
	buildID := fallbackBuildID
	if len(pd.Builds) > 0 {
		sort.Slice(pd.Builds, func(i, j int) bool { return pd.Builds[i].DatePublished > pd.Builds[j].DatePublished })
		for _, b := range pd.Builds {
			if b.Listed && b.ID != 0 {
				buildID = strconv.FormatInt(b.ID, 10)
				break
			}
		}
	}
	var d buildDetails
	u := fmt.Sprintf("https://www.gogdb.org/data/products/%s/builds/%s.json", productID, buildID)
	req, _ = http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "Smurfs2-GOG-Achievement-Repair/1.1.0")
	resp, err = httpClient.Do(req)
	if err != nil {
		return d, buildID, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return d, buildID, fmt.Errorf("GOGDB HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return d, buildID, err
	}
	return d, buildID, nil
}

func authenticate(refreshToken, clientID, clientSecret string) (authResponse, error) {
	var a authResponse
	q := url.Values{}
	q.Set("grant_type", "refresh_token")
	q.Set("refresh_token", refreshToken)
	q.Set("client_id", clientID)
	q.Set("client_secret", clientSecret)
	q.Set("without_new_session", "1")
	req, _ := http.NewRequest("GET", "https://auth.gog.com/token?"+q.Encode(), nil)
	req.Header.Set("User-Agent", "Smurfs2-GOG-Achievement-Repair/1.1.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		return a, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return a, fmt.Errorf("auth HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		return a, err
	}
	if a.AccessToken == "" {
		return a, errors.New("empty access token")
	}
	return a, nil
}

func getAchievements(clientID, userID, accessToken string) ([]achievement, error) {
	u := fmt.Sprintf("https://gameplay.gog.com/clients/%s/users/%s/achievements", clientID, userID)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Gog-Lc", "en")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", "Smurfs2-GOG-Achievement-Repair/1.1.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("achievements HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var ar achievementsResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return nil, err
	}
	return ar.Items, nil
}

func unlockAchievement(clientID, userID, achievementID, accessToken string) (int, error) {
	if achievementID == "" {
		return 0, errors.New("empty achievement ID")
	}
	// IMPORTANT: GOG uses date_unlocked=null to CLEAR an achievement.
	// To unlock, send an explicit timestamp. Match the format used by the
	// reference GOG achievements client and bias it a few seconds into the past.
	when := time.Now().UTC().Add(-3 * time.Second).Format("2006-01-02T15:04:05-0700")
	body, err := json.Marshal(struct {
		DateUnlocked string `json:"date_unlocked"`
	}{DateUnlocked: when})
	if err != nil {
		return 0, err
	}
	u := fmt.Sprintf("https://gameplay.gog.com/clients/%s/users/%s/achievements/%s", clientID, userID, url.PathEscape(achievementID))
	req, _ := http.NewRequest("POST", u, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("User-Agent", "Smurfs2-GOG-Achievement-Repair/1.1.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return resp.StatusCode, nil
}

func achievementByID(achs []achievement, id string) (achievement, bool) {
	for _, a := range achs {
		if a.AchievementID == id {
			return a, true
		}
	}
	return achievement{}, false
}

func waitForUnlocked(clientID, userID, accessToken, achievementID string) (achievement, error) {
	delays := []time.Duration{0, 750 * time.Millisecond, 1500 * time.Millisecond, 2500 * time.Millisecond, 3500 * time.Millisecond}
	var lastErr error
	for _, d := range delays {
		if d > 0 {
			time.Sleep(d)
		}
		achs, err := getAchievements(clientID, userID, accessToken)
		if err != nil {
			lastErr = err
			continue
		}
		a, found := achievementByID(achs, achievementID)
		if !found {
			lastErr = fmt.Errorf("achievement ID %s disappeared from GOG response", achievementID)
			continue
		}
		if a.DateUnlocked != nil {
			return a, nil
		}
		lastErr = errors.New("GOG still reports the achievement as locked")
	}
	if lastErr == nil {
		lastErr = errors.New("GOG did not confirm the achievement unlock")
	}
	return achievement{}, lastErr
}

func valueOrUnknown(p *string) string {
	if p == nil || strings.TrimSpace(*p) == "" {
		return "(unknown)"
	}
	return *p
}

func norm(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	repl := strings.NewReplacer("’", "'", "…", "...", "«", "", "»", "", "“", "", "”", "", "-", " ")
	s = repl.Replace(s)
	return strings.Join(strings.Fields(s), " ")
}
func findAchievementForRule(achs []achievement, r rule) (achievement, bool) {
	for _, a := range achs {
		if strings.EqualFold(a.AchievementKey, r.Key) {
			return a, true
		}
	}
	nn := norm(r.Name)
	for _, a := range achs {
		if norm(a.Name) == nn {
			return a, true
		}
	}
	return achievement{}, false
}
func findMeta(achs []achievement) (achievement, bool) {
	for _, a := range achs {
		n := norm(a.Name)
		d := norm(a.Description)
		if strings.Contains(n, "smurfinator") || strings.Contains(d, "all trophies") || strings.Contains(d, "all achievements") {
			return a, true
		}
	}
	return achievement{}, false
}

// ---------------- UI / repair flow ----------------

type row struct {
	Rule     rule
	Eval     evalResult
	Ach      achievement
	Found    bool
	Unlocked bool
}

func statusLabel(r row) string {
	if r.Unlocked {
		return "UNLOCKED"
	}
	if !r.Found {
		return "INFO"
	}
	switch r.Eval.Status {
	case statusVerified:
		return "VERIFIED"
	case statusInfo:
		return "INFO"
	default:
		return "NOT MET"
	}
}

func setupLog() {
	exe, err := os.Executable()
	dir := "."
	if err == nil {
		dir = filepath.Dir(exe)
	}
	p := filepath.Join(dir, "Smurfs2_GOG_Achievement_Repair.log")
	f, e := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if e == nil {
		logFile = f
		logf("Smurfs2 GOG Achievement Repair %s", toolVersion)
	}
}
func logf(format string, args ...any) {
	if logFile != nil {
		fmt.Fprintf(logFile, time.Now().Format("2006-01-02 15:04:05 ")+format+"\n", args...)
	}
}
func closeLog() {
	if logFile != nil {
		_ = logFile.Close()
	}
}

func findSave() (string, error) {
	var c []string
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		c = append(c, filepath.Join(local, "SM2", "Saved", "SaveGames", "flow.sav"))
	}
	if exe, err := os.Executable(); err == nil {
		c = append(c, filepath.Join(filepath.Dir(exe), "flow.sav"))
	}
	c = append(c, "flow.sav")
	seen := map[string]bool{}
	for _, p := range c {
		if seen[p] {
			continue
		}
		seen[p] = true
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("flow.sav not found in %LOCALAPPDATA%\\SM2\\Saved\\SaveGames or next to this tool")
}

func offlineAudit(path string) error {
	raw, hash, err := parseFlowSave(path)
	if err != nil {
		return err
	}
	slots, err := modelSlots(raw)
	if err != nil {
		return err
	}
	fmt.Printf("[OK] Save: %s\n[OK] SHA-256: %s\n", path, hash)
	fmt.Printf("[OK] Parsed %d save slots (%d used).\n\n", len(slots), len(usedSlots(slots)))
	for _, r := range rules() {
		ev := r.Eval(slots)
		lab := "NOT MET"
		if ev.Status == statusVerified {
			lab = "VERIFIED"
		} else if ev.Status == statusInfo {
			lab = "INFO"
		}
		fmt.Printf("%-10s  %-28s  %s\n", lab, r.Name, ev.Evidence)
	}
	fmt.Println("\nSmurfinator: requires live GOG state, so it is not evaluated offline.")
	return nil
}

func main() {
	setupLog()
	defer closeLog()
	fmt.Println("=================================================================")
	fmt.Printf("The Smurfs 2 - GOG Achievement Repair v%s\n", toolVersion)
	fmt.Println("Achievement repair + optional per-slot save difficulty helper")
	fmt.Println("=================================================================")
	fmt.Println()

	if len(os.Args) >= 2 && os.Args[1] == "--set-difficulty" {
		if len(os.Args) < 4 {
			fatal(errors.New("usage: --set-difficulty <slot> <story|epic|challenge> [flow.sav]"))
		}
		slotNumber, e := strconv.Atoi(os.Args[2])
		if e != nil || slotNumber < 1 {
			fatal(errors.New("invalid slot number"))
		}
		target, ok := difficultyByName(os.Args[3])
		if !ok {
			fatal(errors.New("difficulty must be story, epic, or challenge"))
		}
		savePath := ""
		if len(os.Args) >= 5 {
			savePath = os.Args[4]
		} else {
			savePath, e = findSave()
			if e != nil {
				fatal(e)
			}
		}
		if e := setGameDifficulty(savePath, slotNumber, target); e != nil {
			fatal(e)
		}
		pause()
		return
	}

	savePath, err := findSave()
	if err != nil {
		fatal(err)
	}
	fmt.Println("Choose an action:")
	fmt.Println("  1) Audit / repair GOG achievements")
	fmt.Println("  2) Save helper: change difficulty per slot (Story / Epic / Challenge)")
	fmt.Println("  Q) Quit")
	fmt.Print("Choice [1]: ")
	choice, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	choice = strings.TrimSpace(strings.ToUpper(choice))
	if choice == "Q" {
		return
	}
	if choice == "2" {
		if e := difficultyHelper(savePath); e != nil {
			fatal(e)
		}
		pause()
		return
	}
	if choice != "" && choice != "1" {
		fatal(errors.New("invalid menu choice"))
	}

	rawSlots, hash, err := parseFlowSave(savePath)
	if err != nil {
		fatal(fmt.Errorf("save parse failed: %w", err))
	}
	slots, err := modelSlots(rawSlots)
	if err != nil {
		fatal(fmt.Errorf("save model failed: %w", err))
	}
	fmt.Printf("[OK] Save: %s\n", savePath)
	fmt.Printf("[OK] SHA-256: %s\n", hash)
	fmt.Printf("[OK] %d save slots parsed, %d used.\n", len(slots), len(usedSlots(slots)))
	logf("Save %s SHA256 %s, %d used slots", savePath, hash, len(usedSlots(slots)))
	if len(usedSlots(slots)) == 0 {
		fatal(errors.New("no used save slot found; automatic repair would have no evidence"))
	}

	refreshToken, err := registryRefreshToken()
	if err != nil {
		fatal(fmt.Errorf("GOG Galaxy login token not found: %w", err))
	}
	fmt.Println("[OK] GOG Galaxy login detected. Token is never printed or logged.")
	details, buildID, err := fetchBuildDetails()
	if err != nil {
		fatal(fmt.Errorf("cannot obtain GOG game client data: %w", err))
	}
	if details.ClientID == "" || details.ClientSecret == "" {
		fatal(errors.New("GOG game client data is incomplete"))
	}
	if details.ClientID != expectedClientID {
		fatal(fmt.Errorf("unexpected game client ID %s (expected %s); refusing", details.ClientID, expectedClientID))
	}
	fmt.Printf("[OK] GOG product %s / build %s verified.\n", productID, buildID)
	generalAuth, err := authenticate(refreshToken, galaxyClientID, galaxyClientSecret)
	if err != nil {
		fatal(fmt.Errorf("GOG Galaxy authentication failed: %w", err))
	}
	if generalAuth.UserID == "" {
		fatal(errors.New("GOG authentication returned no user ID"))
	}
	achs, err := getAchievements(details.ClientID, generalAuth.UserID, generalAuth.AccessToken)
	if err != nil {
		fatal(fmt.Errorf("cannot read GOG achievements: %w", err))
	}
	fmt.Printf("[OK] GOG returned %d achievements for this game.\n\n", len(achs))
	logf("GOG returned %d achievements", len(achs))

	rs := rules()
	rows := make([]row, 0, len(rs))
	verifiedMissing := []row{}
	infoMissing := []row{}
	for _, rr := range rs {
		ev := rr.Eval(slots)
		a, found := findAchievementForRule(achs, rr)
		unlocked := found && a.DateUnlocked != nil
		x := row{rr, ev, a, found, unlocked}
		rows = append(rows, x)
		if !unlocked && found && ev.Status == statusVerified {
			verifiedMissing = append(verifiedMissing, x)
		}
		if !unlocked && found && ev.Status == statusInfo {
			infoMissing = append(infoMissing, x)
		}
	}
	// Meta is derived from live GOG state, not from flow.sav.
	meta, metaFound := findMeta(achs)
	metaUnlocked := metaFound && meta.DateUnlocked != nil
	allOthersUnlocked := true
	for _, a := range achs {
		if metaFound && a.AchievementID == meta.AchievementID {
			continue
		}
		if a.DateUnlocked == nil {
			allOthersUnlocked = false
			break
		}
	}

	fmt.Println("STATUS LEGEND: UNLOCKED | VERIFIED | INFO | NOT MET")
	fmt.Println("-----------------------------------------------------------------")
	for _, x := range rows {
		lab := statusLabel(x)
		fmt.Printf("%-9s  %-28s  %s\n", lab, x.Rule.Name, x.Eval.Evidence)
		if !x.Found {
			fmt.Printf("           [INFO] GOG key %s was not returned; no write is possible.\n", x.Rule.Key)
		}
	}
	if metaFound {
		lab := "NOT MET"
		ev := "some other GOG achievements are still locked"
		if metaUnlocked {
			lab = "UNLOCKED"
			ev = "already unlocked on GOG"
		} else if allOthersUnlocked {
			lab = "VERIFIED"
			ev = "all other GOG achievements are unlocked"
		}
		fmt.Printf("%-9s  %-28s  %s\n", lab, "Smurfinator", ev)
	} else {
		fmt.Printf("%-9s  %-28s  %s\n", "INFO", "Smurfinator", "meta achievement not identifiable in GOG response")
	}

	// Add meta to automatic repair only when objectively earned.
	metaNeedsRepair := metaFound && !metaUnlocked && allOthersUnlocked
	if len(verifiedMissing) == 0 && !metaNeedsRepair {
		fmt.Println("\n[OK] No automatically verifiable missing achievement needs repair.")
	} else {
		fmt.Printf("\n[AUTO] %d save-proven achievement(s) can be repaired", len(verifiedMissing))
		if metaNeedsRepair {
			fmt.Print(" + Smurfinator")
		}
		fmt.Println(".")
		fmt.Println("No local save file will be modified.")
		fmt.Print("Type REPAIR then Enter to unlock only VERIFIED achievements (or press Enter to skip): ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.EqualFold(strings.TrimSpace(line), "REPAIR") {
			productAuth, err := authenticate(refreshToken, details.ClientID, details.ClientSecret)
			if err != nil {
				fatal(fmt.Errorf("game-scoped GOG authentication failed: %w", err))
			}
			if productAuth.UserID != "" && productAuth.UserID != generalAuth.UserID {
				fatal(fmt.Errorf("GOG user mismatch between Galaxy token and game-scoped token; refusing write"))
			}
			for _, x := range verifiedMissing {
				fmt.Printf("[ .. ] %s | key=%s | GOG ID=%s\n", x.Rule.Name, x.Rule.Key, x.Ach.AchievementID)
				status, e := unlockAchievement(details.ClientID, generalAuth.UserID, x.Ach.AchievementID, productAuth.AccessToken)
				if e != nil {
					fmt.Printf("[FAIL] %s: write failed: %v\n", x.Rule.Name, e)
					logf("FAIL auto %s id=%s write: %v", x.Rule.Name, x.Ach.AchievementID, e)
					continue
				}
				fmt.Printf("[ .. ] HTTP %d accepted; verifying with GOG...\n", status)
				confirmed, e := waitForUnlocked(details.ClientID, generalAuth.UserID, generalAuth.AccessToken, x.Ach.AchievementID)
				if e != nil {
					fmt.Printf("[FAIL] %s: GOG did not confirm unlock: %v\n", x.Rule.Name, e)
					logf("FAIL auto %s id=%s not confirmed: %v", x.Rule.Name, x.Ach.AchievementID, e)
				} else {
					fmt.Printf("[OK]   %s | confirmed date=%s\n", x.Rule.Name, valueOrUnknown(confirmed.DateUnlocked))
					logf("OK auto %s id=%s confirmed", x.Rule.Name, x.Ach.AchievementID)
				}
			}
			if metaNeedsRepair {
				status, e := unlockAchievement(details.ClientID, generalAuth.UserID, meta.AchievementID, productAuth.AccessToken)
				if e != nil {
					fmt.Printf("[FAIL] Smurfinator: write failed: %v\n", e)
				} else {
					fmt.Printf("[ .. ] Smurfinator HTTP %d accepted; verifying with GOG...\n", status)
					if confirmed, ve := waitForUnlocked(details.ClientID, generalAuth.UserID, generalAuth.AccessToken, meta.AchievementID); ve != nil {
						fmt.Printf("[FAIL] Smurfinator: not confirmed: %v\n", ve)
					} else {
						fmt.Printf("[OK]   Smurfinator | confirmed date=%s\n", valueOrUnknown(confirmed.DateUnlocked))
					}
				}
			}
		} else {
			fmt.Println("Automatic repair skipped.")
		}
	}

	// Refresh live state before manual INFO mode and meta calculation.
	if fresh, e := getAchievements(details.ClientID, generalAuth.UserID, generalAuth.AccessToken); e == nil {
		achs = fresh
	}
	infoMissing = nil
	for _, rr := range rs {
		if !rr.Manual {
			continue
		}
		a, found := findAchievementForRule(achs, rr)
		if found && a.DateUnlocked == nil {
			infoMissing = append(infoMissing, row{Rule: rr, Eval: rr.Eval(slots), Ach: a, Found: true})
		}
	}

	if len(infoMissing) > 0 {
		fmt.Println("\n====================== MANUAL INFO MODE ======================")
		fmt.Println("INFO means the condition cannot be reliably proven from flow.sav.")
		fmt.Println("You may manually repair it ONLY if you genuinely earned it in-game.")
		fmt.Println("Unlocking an achievement you did not actually earn is cheating.")
		fmt.Println("NOT MET achievements are deliberately NOT offered here.")
		fmt.Println("--------------------------------------------------------------")
		for i, x := range infoMissing {
			fmt.Printf("%2d) %-28s  %s\n", i+1, x.Rule.Name, x.Rule.Description)
		}
		fmt.Print("Enter achievement numbers separated by commas, or press Enter to skip manual mode: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		line = strings.TrimSpace(line)
		if line != "" {
			selected := map[int]bool{}
			valid := true
			for _, part := range strings.Split(line, ",") {
				n, e := strconv.Atoi(strings.TrimSpace(part))
				if e != nil || n < 1 || n > len(infoMissing) {
					valid = false
					break
				}
				selected[n-1] = true
			}
			if !valid {
				fmt.Println("[WARN] Invalid selection. Manual mode cancelled.")
			} else {
				fmt.Println("\nWARNING")
				fmt.Println("These INFO achievements cannot be verified from the save.")
				fmt.Println("By continuing, you confirm that YOU actually fulfilled every selected condition in-game.")
				fmt.Print("Type I EARNED THESE to confirm: ")
				confirm, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				c := strings.TrimSpace(confirm)
				if c != "I EARNED THESE" {
					fmt.Println("Manual unlock cancelled. Nothing manual was changed.")
				} else {
					productAuth, err := authenticate(refreshToken, details.ClientID, details.ClientSecret)
					if err != nil {
						fatal(fmt.Errorf("game-scoped GOG authentication failed: %w", err))
					}
					if productAuth.UserID != "" && productAuth.UserID != generalAuth.UserID {
						fatal(fmt.Errorf("GOG user mismatch between Galaxy token and game-scoped token; refusing write"))
					}
					for i := range infoMissing {
						if !selected[i] {
							continue
						}
						x := infoMissing[i]
						fmt.Printf("[ .. ] %s [MANUAL INFO] | key=%s | GOG ID=%s\n", x.Rule.Name, x.Rule.Key, x.Ach.AchievementID)
						status, e := unlockAchievement(details.ClientID, generalAuth.UserID, x.Ach.AchievementID, productAuth.AccessToken)
						if e != nil {
							fmt.Printf("[FAIL] %s: write failed: %v\n", x.Rule.Name, e)
							logf("FAIL manual %s id=%s write: %v", x.Rule.Name, x.Ach.AchievementID, e)
							continue
						}
						fmt.Printf("[ .. ] HTTP %d accepted; verifying with GOG...\n", status)
						confirmed, ve := waitForUnlocked(details.ClientID, generalAuth.UserID, generalAuth.AccessToken, x.Ach.AchievementID)
						if ve != nil {
							fmt.Printf("[FAIL] %s: GOG did not confirm unlock: %v\n", x.Rule.Name, ve)
							logf("FAIL manual %s id=%s not confirmed: %v", x.Rule.Name, x.Ach.AchievementID, ve)
						} else {
							fmt.Printf("[OK]   %s [MANUAL INFO] | confirmed date=%s\n", x.Rule.Name, valueOrUnknown(confirmed.DateUnlocked))
							logf("OK manual INFO %s id=%s confirmed", x.Rule.Name, x.Ach.AchievementID)
						}
					}
				}
			}
		}
	}

	// Final refresh: if every non-meta achievement is now unlocked, repair Smurfinator objectively.
	fresh, err := getAchievements(details.ClientID, generalAuth.UserID, generalAuth.AccessToken)
	if err == nil {
		meta, metaFound = findMeta(fresh)
		if metaFound && meta.DateUnlocked == nil {
			all := true
			for _, a := range fresh {
				if a.AchievementID == meta.AchievementID {
					continue
				}
				if a.DateUnlocked == nil {
					all = false
					break
				}
			}
			if all {
				if productAuth, e := authenticate(refreshToken, details.ClientID, details.ClientSecret); e == nil {
					if _, e = unlockAchievement(details.ClientID, generalAuth.UserID, meta.AchievementID, productAuth.AccessToken); e == nil {
						if confirmed, ve := waitForUnlocked(details.ClientID, generalAuth.UserID, generalAuth.AccessToken, meta.AchievementID); ve == nil {
							fmt.Printf("[OK]   Smurfinator repaired and confirmed: %s\n", valueOrUnknown(confirmed.DateUnlocked))
							logf("OK meta Smurfinator confirmed")
						} else {
							fmt.Printf("[FAIL] Smurfinator write was not confirmed: %v\n", ve)
						}
					}
				}
			}
		}
	}

	fmt.Println("\nDone. Achievement repair mode did not modify flow.sav.")
	fmt.Println("Only [OK] lines with a confirmed date are considered successful.")
	fmt.Println("Refresh the game's Achievements page in GOG Galaxy. If the UI is stale, restart Galaxy.")
	pause()
}

func fatal(err error) {
	fmt.Printf("\n[ERROR] %v\n", err)
	fmt.Println("No local save was modified. No further achievement write was attempted after this error.")
	logf("ERROR %v", err)
	pause()
	os.Exit(1)
}
func pause() {
	fmt.Print("\nPress Enter to close...")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}
