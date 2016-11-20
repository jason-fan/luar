// Copyright (c) 2010-2016 Steve Donovan

package luar

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/aarzilli/golua/lua"
)

type LuaToGoError struct {
	Lua string
	Go  reflect.Value
}

func (l LuaToGoError) Error() string {
	return fmt.Sprintf("cannot convert %v to %v", l.Lua, l.Go.Type())
}

// NullT is the type of Null.
// Having a dedicated type allows us to make the distinction between zero values and Null.
type NullT int

// Map is an alias for map of strings.
type Map map[string]interface{}

var (
	// Null is the definition of 'luar.null' which is used in place of 'nil' when
	// converting slices and structs.
	Null = NullT(0)
)

var (
	tslice = typeof((*[]interface{})(nil))
	tmap   = typeof((*map[string]interface{})(nil))
	nullv  = reflect.ValueOf(Null)
)

// visitor holds the index to the table in LUA_REGISTRYINDEX with all the tables
// we ran across during a GoToLua conversion.
type visitor struct {
	L     *lua.State
	index int
}

func newVisitor(L *lua.State) visitor {
	var v visitor
	v.L = L
	v.L.NewTable()
	v.index = v.L.Ref(lua.LUA_REGISTRYINDEX)
	return v
}

func (v *visitor) close() {
	v.L.Unref(lua.LUA_REGISTRYINDEX, v.index)
}

// Mark value on top of the stack as visited using the registry index.
func (v *visitor) mark(val reflect.Value) {
	ptr := val.Pointer()
	v.L.RawGeti(lua.LUA_REGISTRYINDEX, v.index)
	// Copy value on top.
	v.L.PushValue(-2)
	// Set value to table.
	v.L.RawSeti(-2, int(ptr))
	v.L.Pop(1)
}

// Push visited value on top of the stack.
// If the value was not visited, return false and push nothing.
func (v *visitor) push(val reflect.Value) bool {
	ptr := val.Pointer()
	v.L.RawGeti(lua.LUA_REGISTRYINDEX, v.index)
	v.L.RawGeti(-1, int(ptr))
	if v.L.IsNil(-1) {
		// Not visited.
		v.L.Pop(2)
		return false
	}
	v.L.Replace(-2)
	return true
}

// Init makes and initialize a new pre-configured Lua state.
//
// It populates the 'luar' table with some helper functions/values:
//
//   method: ProxyMethod
//   type: ProxyType
//   unproxify: Unproxify
//
//   chan: MakeChan
//   complex: MakeComplex
//   map: MakeMap
//   slice: MakeSlice
//
//   null: Null
//
// It replaces the pairs/ipairs functions so that __pairs/__ipairs can be used,
// Lua 5.2 style. It allows for looping over Go composite types and strings.
//
// It is not required for using the 'GoToLua' and 'LuaToGo' functions.
func Init() *lua.State {
	var L = lua.NewState()
	L.OpenLibs()
	Register(L, "luar", Map{
		// Functions.
		"unproxify": Unproxify,

		"method": ProxyMethod,
		"type":   ProxyType, // TODO: Replace with the version from the 'proxytype' branch.

		"chan":    MakeChan,
		"complex": Complex,
		"map":     MakeMap,
		"slice":   MakeSlice,

		// Values.
		"null": Null,
	})
	Register(L, "", Map{
		"ipairs": ProxyIpairs,
		"pairs":  ProxyPairs,
	})
	return L
}

func isNil(v reflect.Value) bool {
	nullables := [...]bool{
		reflect.Chan:      true,
		reflect.Func:      true,
		reflect.Interface: true,
		reflect.Map:       true,
		reflect.Ptr:       true,
		reflect.Slice:     true,
	}

	kind := v.Type().Kind()
	if int(kind) >= len(nullables) {
		return false
	}
	return nullables[kind] && v.IsNil()
}

func copyMapToTable(L *lua.State, vmap reflect.Value, visited visitor) {
	n := vmap.Len()
	L.CreateTable(0, n)
	visited.mark(vmap)
	for _, key := range vmap.MapKeys() {
		v := vmap.MapIndex(key)
		goToLua(L, key, true, visited)
		if isNil(v) {
			v = nullv
		}
		goToLua(L, v, false, visited)
		L.SetTable(-3)
	}
}

// Also for arrays.
func copySliceToTable(L *lua.State, vslice reflect.Value, visited visitor) {
	ref := vslice
	for vslice.Kind() == reflect.Ptr {
		// For arrays.
		vslice = vslice.Elem()
	}

	n := vslice.Len()
	L.CreateTable(n, 0)
	if vslice.Kind() == reflect.Slice {
		visited.mark(vslice)
	} else if ref.Kind() == reflect.Ptr {
		visited.mark(ref)
	}

	for i := 0; i < n; i++ {
		L.PushInteger(int64(i + 1))
		v := vslice.Index(i)
		if isNil(v) {
			v = nullv
		}
		goToLua(L, v, false, visited)
		L.SetTable(-3)
	}
}

func copyStructToTable(L *lua.State, vstruct reflect.Value, visited visitor) {
	// If 'vstruct' is a pointer to struct, use the pointer to mark as visited.
	ref := vstruct
	for vstruct.Kind() == reflect.Ptr {
		vstruct = vstruct.Elem()
	}

	n := vstruct.NumField()
	L.CreateTable(n, 0)
	if ref.Kind() == reflect.Ptr {
		visited.mark(ref)
	}

	for i := 0; i < n; i++ {
		st := vstruct.Type()
		field := st.Field(i)
		key := field.Name
		tag := field.Tag.Get("lua")
		if tag != "" {
			key = tag
		}
		goToLua(L, key, false, visited)
		v := vstruct.Field(i)
		goToLua(L, v, false, visited)
		L.SetTable(-3)
	}
}

func callGo(L *lua.State, funv reflect.Value, args []reflect.Value) []reflect.Value {
	defer func() {
		if x := recover(); x != nil {
			RaiseError(L, "error %s", x)
		}
	}()
	resv := funv.Call(args)
	return resv
}

func goLuaFunc(L *lua.State, fun reflect.Value) lua.LuaGoFunction {
	switch f := fun.Interface().(type) {
	case func(*lua.State) int:
		return f
	}

	funT := fun.Type()
	tArgs := make([]reflect.Type, funT.NumIn())
	for i := range tArgs {
		tArgs[i] = funT.In(i)
	}

	return func(L *lua.State) int {
		var lastT reflect.Type
		origTArgs := tArgs
		isVariadic := funT.IsVariadic()

		if isVariadic {
			n := len(tArgs)
			lastT = tArgs[n-1].Elem()
			tArgs = tArgs[0 : n-1]
		}

		args := make([]reflect.Value, len(tArgs))
		for i, t := range tArgs {
			val := reflect.New(t)
			err := LuaToGo(L, i+1, val.Interface())
			if err != nil {
				RaiseError(L, "cannot convert go function arguments: %v", err)
			}
			args[i] = val.Elem()
		}

		if isVariadic {
			n := L.GetTop()
			for i := len(tArgs) + 1; i <= n; i++ {
				val := reflect.New(lastT)
				err := LuaToGo(L, i, val.Interface())
				if err != nil {
					RaiseError(L, "cannot convert go function arguments: %v", err)
				}
				args = append(args, val.Elem())
			}
			tArgs = origTArgs
		}
		resV := callGo(L, fun, args)
		for _, val := range resV {
			if val.Kind() == reflect.Struct {
				// If the function returns a struct (and not a pointer to a struct),
				// calling GoToLua directly will convert it to a table, making the
				// mathods inaccessible. We work around that issue by forcibly passing a
				// pointer to a struct.
				n := reflect.New(val.Type())
				n.Elem().Set(val)
				val = n
			}
			GoToLuaProxy(L, val)
		}
		return len(resV)
	}
}

// GoToLua pushes a Go value 'val' on the Lua stack.
//
// It unboxes interfaces.
//
// Pointers are followed recursively. Slices, structs and maps are copied over as tables.
func GoToLua(L *lua.State, val interface{}) {
	v := newVisitor(L)
	goToLua(L, val, false, v)
	v.close()
}

// GoToLuaProxy is like GoToLua but pushes a proxy on the Lua stack when it makes sense.
//
// A proxy is a Lua userdata that wraps a Go value.
//
// Pointers are preserved.
//
// Structs and arrays need to be passed as pointers to be proxified, otherwise
// they will be copied as tables.
//
// Predeclared scalar types are never proxified as they have no methods.
func GoToLuaProxy(L *lua.State, val interface{}) {
	v := newVisitor(L)
	goToLua(L, val, true, v)
	v.close()
}

// TODO: Check if we really need multiple pointer levels since pointer methods
// can be called on non-pointers.
func goToLua(L *lua.State, v interface{}, proxify bool, visited visitor) {
	var val reflect.Value
	val, ok := v.(reflect.Value)
	if !ok {
		val = reflect.ValueOf(v)
	}
	if !val.IsValid() {
		L.PushNil()
		return
	}

	// Unbox interface.
	if val.Kind() == reflect.Interface && !val.IsNil() {
		val = reflect.ValueOf(val.Interface())
	}

	// Follow pointers if not proxifying. We save the original pointer Value in case we proxify.
	ptrVal := val
	for val.Kind() == reflect.Ptr {
		val = val.Elem()
	}

	if !val.IsValid() {
		L.PushNil()
		return
	}

	// As a special case, we always proxify Null, the empty element for slices and maps.
	if val.CanInterface() && val.Interface() == Null {
		makeValueProxy(L, val, cInterfaceMeta)
		return
	}

	switch val.Kind() {
	case reflect.Float64, reflect.Float32:
		if proxify && isNewType(val.Type()) {
			makeValueProxy(L, ptrVal, cNumberMeta)
		} else {
			L.PushNumber(val.Float())
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if proxify && isNewType(val.Type()) {
			makeValueProxy(L, ptrVal, cNumberMeta)
		} else {
			L.PushNumber(float64(val.Int()))
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if proxify && isNewType(val.Type()) {
			makeValueProxy(L, ptrVal, cNumberMeta)
		} else {
			L.PushNumber(float64(val.Uint()))
		}
	case reflect.String:
		if proxify && isNewType(val.Type()) {
			makeValueProxy(L, ptrVal, cStringMeta)
		} else {
			L.PushString(val.String())
		}
	case reflect.Bool:
		if proxify && isNewType(val.Type()) {
			makeValueProxy(L, ptrVal, cInterfaceMeta)
		} else {
			L.PushBoolean(val.Bool())
		}
	case reflect.Complex128, reflect.Complex64:
		makeValueProxy(L, ptrVal, cComplexMeta)
	case reflect.Array:
		// It needs be a pointer to be a proxy, otherwise values won't be settable.
		if proxify && ptrVal.Kind() == reflect.Ptr {
			makeValueProxy(L, ptrVal, cSliceMeta)
		} else {
			// See the case of struct.
			if ptrVal.Kind() == reflect.Ptr && visited.push(ptrVal) {
				return
			}
			copySliceToTable(L, ptrVal, visited)
		}
	case reflect.Slice:
		if proxify {
			makeValueProxy(L, ptrVal, cSliceMeta)
		} else {
			if visited.push(val) {
				return
			}
			copySliceToTable(L, val, visited)
		}
	case reflect.Map:
		if proxify {
			makeValueProxy(L, ptrVal, cMapMeta)
		} else {
			if visited.push(val) {
				return
			}
			copyMapToTable(L, val, visited)
		}
	case reflect.Struct:
		if proxify && ptrVal.Kind() == reflect.Ptr {
			if ptrVal.CanInterface() {
				switch v := ptrVal.Interface().(type) {
				case error:
					L.PushString(v.Error())
				case *LuaObject:
					v.Push()
				default:
					makeValueProxy(L, ptrVal, cStructMeta)
				}
			} else {
				makeValueProxy(L, ptrVal, cStructMeta)
			}
		} else {
			// Use ptrVal instead of val to detect cycles from the very first element, if a pointer.
			if ptrVal.Kind() == reflect.Ptr && visited.push(ptrVal) {
				return
			}
			copyStructToTable(L, ptrVal, visited)
		}
	case reflect.Chan:
		makeValueProxy(L, ptrVal, cChannelMeta)
	case reflect.Func:
		L.PushGoFunction(goLuaFunc(L, val))
	default:
		if v, ok := val.Interface().(error); ok {
			L.PushString(v.Error())
		} else if val.IsNil() {
			L.PushNil()
		} else {
			makeValueProxy(L, ptrVal, cInterfaceMeta)
		}
	}
}

func luaIsEmpty(L *lua.State, idx int) bool {
	L.PushNil()
	if idx < 0 {
		idx--
	}
	if L.Next(idx) != 0 {
		L.Pop(2)
		return false
	}
	return true
}

func copyTableToMap(L *lua.State, idx int, value reflect.Value, visited map[uintptr]interface{}) error {
	t := value.Type()
	if value.Kind() == reflect.Interface {
		t = tmap
	}
	te, tk := t.Elem(), t.Key()
	m := reflect.MakeMap(t)

	// See copyTableToSlice.
	ptr := L.ToPointer(idx)
	if !luaIsEmpty(L, idx) {
		visited[ptr] = m.Interface()
	}

	L.PushNil()
	if idx < 0 {
		idx--
	}
	for L.Next(idx) != 0 {
		// key at -2, value at -1
		key := reflect.New(tk).Elem()
		luaToGo(L, -2, key, visited)
		val := reflect.New(te).Elem()
		luaToGo(L, -1, val, visited)
		if val.Interface() == Null {
			val = reflect.Zero(te)
		}
		m.SetMapIndex(key, val)
		L.Pop(1)
	}

	value.Set(m)
	// TODO: Return nothing as it always succeeds? Make sure of that. Compare to other copy* funcs.
	return nil
}

// Also for arrays.
func copyTableToSlice(L *lua.State, idx int, value reflect.Value, visited map[uintptr]interface{}) error {
	t := value.Type()
	if value.Kind() == reflect.Interface {
		t = tslice
	}

	n := int(L.ObjLen(idx))

	// TODO: Rename 'slice'. Rename 'value', 'vstruct', etc.
	var slice reflect.Value
	if t.Kind() == reflect.Array {
		slice = reflect.New(t)
		slice = slice.Elem()
	} else {
		slice = reflect.MakeSlice(t, n, n)
	}

	// Do not add empty slices to the list of visited elements.
	// The empty Lua table is a single instance object and gets re-used across maps, slices and others.
	// Arrays cannot be cyclic since the interface type will ask for slices.
	if n > 0 && t.Kind() != reflect.Array {
		ptr := L.ToPointer(idx)
		visited[ptr] = slice.Interface()
	}

	te := t.Elem()
	for i := 1; i <= n; i++ {
		L.RawGeti(idx, i)
		val := reflect.New(te).Elem()
		luaToGo(L, -1, val, visited)
		if val.Interface() == Null {
			val = reflect.Zero(te)
		}
		slice.Index(i - 1).Set(val)
		L.Pop(1)
	}

	value.Set(slice)
	return nil
}

func copyTableToStruct(L *lua.State, idx int, value reflect.Value, visited map[uintptr]interface{}) error {
	t := value.Type()
	// TODO: Use on 'value' directly? Yes.
	ref := reflect.New(t).Elem()

	// See copyTableToSlice.
	ptr := L.ToPointer(idx)
	if !luaIsEmpty(L, idx) {
		// TODO: If we don't handle pointers, then no need for visited.
		visited[ptr] = ref.Addr().Interface()
	}

	// Associate Lua keys with Go fields: tags have priority over matching field
	// name.
	fields := map[string]string{}
	st := ref.Type()
	for i := 0; i < ref.NumField(); i++ {
		field := st.Field(i)
		tag := field.Tag.Get("lua")
		if tag != "" {
			fields[tag] = field.Name
			continue
		}
		fields[field.Name] = field.Name
	}

	L.PushNil()
	if idx < 0 {
		idx--
	}
	for L.Next(idx) != 0 {
		key := L.ToString(-2)
		f := ref.FieldByName(fields[key])
		if f.CanSet() && f.IsValid() {
			val := reflect.New(f.Type()).Elem()
			luaToGo(L, -1, val, visited)
			f.Set(val)
		}
		L.Pop(1)
	}

	value.Set(ref)
	return nil
}

// LuaToGo converts the Lua value at index 'idx' to the Go value.
// Handles numerical and string types in a straightforward way, and will convert
// tables to either map or slice types.
// Return an error if 'v' is not a non-nil pointer.
func LuaToGo(L *lua.State, idx int, v interface{}) error {
	// TODO: For now, unwrapping proxies require the same type. If we keep that behaviour, document it.
	value := reflect.ValueOf(v)
	// TODO: Allow unreferenced map? json does not do it...
	if value.Kind() != reflect.Ptr {
		return errors.New("not a pointer")
	}
	value = value.Elem()
	return luaToGo(L, idx, value, map[uintptr]interface{}{})
}

func luaToGo(L *lua.State, idx int, value reflect.Value, visited map[uintptr]interface{}) error {
	kind := value.Kind()

	switch L.Type(idx) {
	case lua.LUA_TNIL:
		value.Set(reflect.Zero(value.Type()))
	case lua.LUA_TBOOLEAN:
		if kind != reflect.Bool && kind != reflect.Interface {
			return LuaToGoError{Lua: L.LTypename(idx), Go: value}
		}
		value.Set(reflect.ValueOf(L.ToBoolean(idx)))
	case lua.LUA_TSTRING:
		if kind != reflect.String && kind != reflect.Interface {
			return LuaToGoError{Lua: L.LTypename(idx), Go: value}
		}
		value.Set(reflect.ValueOf(L.ToString(idx)))
	case lua.LUA_TNUMBER:
		switch k := unsizedKind(value); k {
		case reflect.Int64, reflect.Uint64, reflect.Float64:
			f := reflect.ValueOf(L.ToNumber(idx)).Convert(value.Type())
			value.Set(f)
		case reflect.Interface:
			// TODO: Merge with other numbers? Check if conversion does something.
			value.Set(reflect.ValueOf(L.ToNumber(idx)))
		case reflect.Complex128:
			value.SetComplex(complex(L.ToNumber(idx), 0))
		default:
			return LuaToGoError{Lua: L.LTypename(idx), Go: value}
		}
	case lua.LUA_TUSERDATA:
		if isValueProxy(L, idx) {
			v, t := valueOfProxy(L, idx)
			if v.Interface() == Null {
				// Special case for Null.
				value.Set(reflect.Zero(value.Type()))
				return nil
			}
			if !t.ConvertibleTo(value.Type()) {
				return LuaToGoError{Lua: t.String(), Go: value}
			}
			// We automatically convert between types. This behaviour is consistent
			// with LuaToGo conversions elsewhere.
			value.Set(v.Convert(value.Type()))
			return nil
		} else if kind != reflect.Interface || value.Type() != reflect.TypeOf(LuaObject{}) {
			return LuaToGoError{Lua: L.LTypename(idx), Go: value}
		}
		// Wrap the userdata into a LuaObject.
		value.Set(reflect.ValueOf(NewLuaObject(L, idx)))
	case lua.LUA_TTABLE:
		// TODO: Check what happens if visited is not of the right type.
		// TODO: Check cyclic arrays / structs.
		// TODO: visited should hold reflect.Values.
		ptr := L.ToPointer(idx)
		if val, ok := visited[ptr]; ok {
			v := reflect.ValueOf(val)
			value.Set(v)
			return nil
		}
		switch kind {
		case reflect.Array:
			fallthrough
		case reflect.Slice:
			return copyTableToSlice(L, idx, value, visited)
		case reflect.Map:
			return copyTableToMap(L, idx, value, visited)
		case reflect.Struct:
			return copyTableToStruct(L, idx, value, visited)
		case reflect.Interface:
			// We have to make an executive decision here: tables with non-zero
			// length are assumed to be slices!
			// TODO: Bad! Instead, compare the count of element with the Lua length of the table.
			if L.ObjLen(idx) > 0 {
				return copyTableToSlice(L, idx, value, visited)
			} else {
				return copyTableToMap(L, idx, value, visited)
			}
		default:
			return LuaToGoError{Lua: L.LTypename(idx), Go: value}
		}
	default:
		return LuaToGoError{Lua: L.LTypename(idx), Go: value}
	}

	return nil
}

func isNewType(t reflect.Type) bool {
	types := [...]reflect.Type{
		reflect.Invalid:    nil, // Invalid Kind = iota
		reflect.Bool:       typeof((*bool)(nil)),
		reflect.Int:        typeof((*int)(nil)),
		reflect.Int8:       typeof((*int8)(nil)),
		reflect.Int16:      typeof((*int16)(nil)),
		reflect.Int32:      typeof((*int32)(nil)),
		reflect.Int64:      typeof((*int64)(nil)),
		reflect.Uint:       typeof((*uint)(nil)),
		reflect.Uint8:      typeof((*uint8)(nil)),
		reflect.Uint16:     typeof((*uint16)(nil)),
		reflect.Uint32:     typeof((*uint32)(nil)),
		reflect.Uint64:     typeof((*uint64)(nil)),
		reflect.Uintptr:    typeof((*uintptr)(nil)),
		reflect.Float32:    typeof((*float32)(nil)),
		reflect.Float64:    typeof((*float64)(nil)),
		reflect.Complex64:  typeof((*complex64)(nil)),
		reflect.Complex128: typeof((*complex128)(nil)),
		reflect.String:     typeof((*string)(nil)),
	}

	pt := types[int(t.Kind())]
	return pt != t
}

// RaiseError raises a Lua error from Go code.
func RaiseError(L *lua.State, format string, args ...interface{}) {
	// TODO: Rename to Fatalf?
	// TODO: Don't use and always return errors? Test what happens in examples. Can we continue?
	// TODO: Use golua's RaiseError?
	L.Where(1)
	pos := L.ToString(-1)
	L.Pop(1)
	panic(L.NewError(pos + fmt.Sprintf(format, args...)))
}

// Register makes a number of Go values available in Lua code.
// 'values' is a map of strings to Go values.
//
// - If table is non-nil, then create or reuse a global table of that name and
// put the values in it.
//
// - If table is '' then put the values in the global table (_G).
//
// - If table is '*' then assume that the table is already on the stack.
func Register(L *lua.State, table string, values Map) {
	pop := true
	if table == "*" {
		pop = false
	} else if len(table) > 0 {
		L.GetGlobal(table)
		if L.IsNil(-1) {
			L.NewTable()
			L.SetGlobal(table)
			L.GetGlobal(table)
		}
	} else {
		L.GetGlobal("_G")
	}
	for name, val := range values {
		GoToLuaProxy(L, val)
		L.SetField(-2, name)
	}
	if pop {
		L.Pop(1)
	}
}

func assertValid(L *lua.State, v reflect.Value, parent reflect.Value, name string, what string) {
	if !v.IsValid() {
		RaiseError(L, "no %s named `%s` for type %s", what, name, parent.Type())
	}
}

// Closest we'll get to a typeof operator.
func typeof(v interface{}) reflect.Type {
	return reflect.TypeOf(v).Elem()
}
