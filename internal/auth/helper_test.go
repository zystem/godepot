package auth_test

import "github.com/zystem/godepot/internal/meta"

func mustStmt(sql string) meta.Stmt { return meta.S(sql) }
