// Code generated by 'yaegi extract github.com/xo/xo/templates/jsontpl'. DO NOT EDIT.

package internal

import (
	"github.com/xo/xo/templates/jsontpl"
	"reflect"
)

func init() {
	Symbols["github.com/xo/xo/templates/jsontpl/jsontpl"] = map[string]reflect.Value{
		// function, constant and variable definitions
		"Files":     reflect.ValueOf(&jsontpl.Files).Elem(),
		"Indent":    reflect.ValueOf(jsontpl.Indent),
		"IndentKey": reflect.ValueOf(jsontpl.IndentKey),
		"Ugly":      reflect.ValueOf(jsontpl.Ugly),
		"UglyKey":   reflect.ValueOf(jsontpl.UglyKey),
	}
}
