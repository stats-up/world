package dbmigrate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// Collation es una intercalación que se ofrece al crear una base (siempre utf8mb4).
type Collation struct{ Value, Label string }

var Collations = []Collation{
	{"utf8mb4_unicode_ci", "utf8mb4_unicode_ci · la de Laravel por defecto"},
	{"utf8mb4_0900_ai_ci", "utf8mb4_0900_ai_ci · la de MySQL 8 (MariaDB la admite desde 11.4)"},
	{"utf8mb4_general_ci", "utf8mb4_general_ci · la de MariaDB y servidores antiguos"},
	{"utf8mb4_bin", "utf8mb4_bin · distingue mayúsculas y acentos"},
}

func validCollation(c string) bool {
	for _, o := range Collations {
		if o.Value == c {
			return true
		}
	}
	return false
}

// CreateEmptyDatabase crea una base vacía (utf8mb4) desde el explorador; falla si ya existe.
func CreateEmptyDatabase(ctx context.Context, c Conn, name, collation string) error {
	if !ValidNewName(name) || systemDB(name) {
		return errors.New("nombre inválido: usa letras, números y guion bajo (máx. 64)")
	}
	if !validCollation(collation) {
		return errors.New("intercalación inválida")
	}
	c.Database = ""
	db, err := open(ctx, c)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, "CREATE DATABASE "+quoteIdent(name)+" CHARACTER SET utf8mb4 COLLATE "+collation)
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		switch me.Number {
		case 1007: // ER_DB_CREATE_EXISTS
			return fmt.Errorf("ya existe una base llamada %s", name)
		case 1044, 1045: // sin permiso
			return fmt.Errorf("el usuario %s no tiene permiso para crear bases", c.User)
		case 1273: // ER_UNKNOWN_COLLATION
			return fmt.Errorf("este servidor no tiene la intercalación %s; elige otra", collation)
		}
		return errors.New(strings.TrimSpace(me.Message))
	}
	return err
}
