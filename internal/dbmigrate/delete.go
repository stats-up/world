package dbmigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// MaxDelete es el máximo de registros por eliminación (una página de la grilla).
const MaxDelete = 200

// DeleteRows elimina registros identificados por su clave (Row.Key), todos o ninguno:
// corre en una transacción y, si la base rechaza alguno (p. ej. por una clave foránea),
// no se elimina nada. Devuelve cuántos se eliminaron (pueden ser menos si alguno ya no existía).
func DeleteRows(ctx context.Context, c Conn, db, table string, keys []string) (int64, error) {
	if len(keys) == 0 {
		return 0, errors.New("no se eligió ningún registro")
	}
	if len(keys) > MaxDelete {
		return 0, fmt.Errorf("se pueden eliminar hasta %d registros a la vez", MaxDelete)
	}
	if systemDB(db) {
		return 0, errors.New("no se eliminan registros de las bases del sistema")
	}
	c.Database = ""
	pool, err := open(ctx, c)
	if err != nil {
		return 0, err
	}
	defer pool.Close()
	conn, err := pool.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	conn.ExecContext(ctx, fmt.Sprintf(`SET SESSION max_execution_time = %d`, queryTimeMS))
	conn.ExecContext(ctx, fmt.Sprintf(`SET SESSION max_statement_time = %d`, queryTimeMS/1000))
	// Si un registro está tomado por otra transacción, mejor fallar pronto que esperar 50 s.
	conn.ExecContext(ctx, `SET SESSION innodb_lock_wait_timeout = 10`)

	t, err := tableDetail(ctx, conn, db, table)
	if err != nil {
		return 0, err
	}
	if t.View() {
		return 0, errors.New("es una vista: elimina los registros en su tabla")
	}
	key := t.Key()
	if len(key) == 0 {
		return 0, errors.New("la tabla no tiene clave primaria ni un índice único: no se puede identificar cada registro")
	}
	where := make([]string, len(key))
	for i, col := range key {
		if col.Binary() {
			where[i] = quoteIdent(col.Name) + " = UNHEX(?)"
		} else {
			where[i] = quoteIdent(col.Name) + " = ?"
		}
	}
	query := "DELETE FROM " + quoteIdent(db) + "." + quoteIdent(table) + " WHERE " + strings.Join(where, " AND ") + " LIMIT 1"

	args := make([][]any, len(keys))
	for i, k := range keys {
		var vals []string
		if err := json.Unmarshal([]byte(k), &vals); err != nil || len(vals) != len(key) {
			return 0, errors.New("registro inválido: recarga la página e intenta de nuevo")
		}
		args[i] = make([]any, len(vals))
		for j, v := range vals {
			args[i][j] = v
		}
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	var n int64
	for _, a := range args {
		res, err := stmt.ExecContext(ctx, a...)
		if err != nil {
			return 0, deleteError(err, t)
		}
		affected, _ := res.RowsAffected()
		n += affected
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// deleteError explica los errores más comunes al eliminar.
func deleteError(err error, t *TableDetail) error {
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		return err
	}
	switch me.Number {
	case 1451: // ER_ROW_IS_REFERENCED_2
		var refs []string
		for _, r := range t.References {
			if r.Blocks() {
				refs = append(refs, r.Table+"."+r.Column)
			}
		}
		msg := "otros registros dependen de este"
		if len(refs) > 0 {
			msg += " (" + strings.Join(refs, ", ") + ")"
		}
		return errors.New(msg + ": elimina o cambia primero esos registros. No se eliminó nada.")
	case 1205: // ER_LOCK_WAIT_TIMEOUT
		return errors.New("el registro está siendo modificado por otra conexión; intenta de nuevo en un momento. No se eliminó nada.")
	case 1142, 1044: // sin permiso
		return fmt.Errorf("el usuario de la conexión no tiene permiso para eliminar en esta tabla (%s)", me.Message)
	}
	return err
}
