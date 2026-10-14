package store

import (
	"database/sql"
	"encoding/json"
)

// NullString 可空字符串：JSON 输出为 string 或 null（干净契约），DB 侧包装 sql.NullString。
type NullString struct {
	sql.NullString
}

// NewNullString 构造。
func NewNullString(s string) NullString {
	return NullString{sql.NullString{String: s, Valid: s != ""}}
}

// MarshalJSON string 或 null。
func (n NullString) MarshalJSON() ([]byte, error) {
	if !n.Valid {
		return []byte("null"), nil
	}
	return json.Marshal(n.String)
}

// UnmarshalJSON 接受 string 或 null。
func (n *NullString) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		n.Valid = false
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	n.String = s
	n.Valid = true
	return nil
}

// Value / Scan 由 sql.NullString 内嵌提供；Value 需要显式导出以防驱动取零值。
func (n NullString) Value() (any, error) {
	if !n.Valid {
		return nil, nil
	}
	return n.String, nil
}

// Ptr 转 *string（api 层可选使用）。
func (n NullString) Ptr() *string {
	if !n.Valid {
		return nil
	}
	s := n.String
	return &s
}
