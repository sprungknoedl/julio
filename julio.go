package julio

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/Masterminds/squirrel"
	"github.com/lib/pq"
)

var psql = squirrel.StatementBuilder.PlaceholderFormat(squirrel.Dollar)

const prefix = "julio_notify"

type Filter struct {
	Sqlizer squirrel.Sqlizer
	Offset  uint64
	Updates bool
}

type Julio struct {
	DB         *sql.DB
	dataSource string
}

func Open(dataSource string) (*Julio, error) {
	db, err := sql.Open("postgres", dataSource)
	if err != nil {
		return nil, err
	}

	return &Julio{
		DB:         db,
		dataSource: dataSource,
	}, err
}

func (j *Julio) Init(table string) error {
	query := `
		CREATE OR REPLACE FUNCTION <PREFIX>_<TABLE>() RETURNS TRIGGER AS $$
			BEGIN
				PERFORM pg_notify('<PREFIX>_<TABLE>', NEW.id::text);
				RETURN NULL;
			END;
		$$ LANGUAGE plpgsql;
		
		CREATE TABLE IF NOT EXISTS <TABLE> (
			id BIGSERIAL PRIMARY KEY,
			data JSONB NOT NULL
		);

		CREATE INDEX IF NOT EXISTS <TABLE>_data_idx
			ON <TABLE>
			USING gin
			(data jsonb_path_ops);

		DROP TRIGGER IF EXISTS <PREFIX> ON <TABLE>;
		CREATE TRIGGER <PREFIX>
			AFTER INSERT ON <TABLE>
			FOR EACH ROW EXECUTE PROCEDURE <PREFIX>_<TABLE>();`
	query = strings.Replace(query, "<TABLE>", table, -1)
	query = strings.Replace(query, "<PREFIX>", prefix, -1)
	_, err := j.DB.Exec(query)
	if err != nil {
		return err
	}

	return nil
}

func (j *Julio) Add(table string, v interface{}) (int, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}

	query, args, err := psql.
		Insert(table).
		Columns("data").
		Values(data).
		Suffix("RETURNING id").
		ToSql()
	if err != nil {
		return 0, err
	}

	id := 0
	err = j.DB.QueryRow(query, args...).Scan(&id)
	return id, err
}

func (j *Julio) Get(table string, filter Filter) *Rows {
	rows := &Rows{
		C:       make(chan Row, 1024),
		backlog: make(chan Row, 1024),
		done:    make(chan struct{}),

		julio:  j,
		table:  table,
		filter: filter,
	}

	go rows.notifyloop()
	go rows.selectloop()
	return rows
}

type Row struct {
	ID   int
	Data json.RawMessage
}

type Rows struct {
	C   chan Row
	Err error

	julio   *Julio
	table   string
	filter  Filter
	backlog chan Row
	done    chan struct{}
}

func (r *Rows) notifyloop() {
	defer close(r.backlog)
	if !r.filter.Updates {
		return
	}

	// goroutine leak debug logs
	// log.Printf("notify loop started: %p", r)
	// defer log.Printf("notify loop closed: %p", r)

	listener := pq.NewListener(r.julio.dataSource,
		10*time.Second,
		1*time.Minute,
		func(ev pq.ListenerEventType, err error) {})
	err := listener.Listen(prefix + "_" + r.table)
	if err != nil {
		r.Err = err
		return
	}

	defer listener.Close()
	for {
		select {
		case <-r.done:
			return

		case n := <-listener.Notify:
			id, err := strconv.Atoi(n.Extra)
			if err != nil {
				r.Err = err
				return
			}

			query, args, err := psql.
				Select("id", "data").
				From(r.table).
				Where(squirrel.And{
					squirrel.Eq{"id": id},
					r.filter.Sqlizer}).
				OrderBy("id").
				ToSql()
			if err != nil {
				r.Err = err
				return
			}

			rows, err := r.julio.DB.Query(query, args...)
			if err != nil {
				r.Err = err
				return
			}

			defer rows.Close()
			for rows.Next() {
				event := Row{}
				err := rows.Scan(&event.ID, &event.Data)
				if err != nil {
					r.Err = err
					return
				}

				select {
				case r.backlog <- event:
				case <-r.done:
					return
				}

			}

			if rows.Err() != nil {
				r.Err = rows.Err()
				return
			}

		case <-time.After(90 * time.Second):
			go listener.Ping()
		}
	}
}

func (r *Rows) selectloop() {
	// goroutine leak debug logs
	// log.Printf("select loop started: %p", r)
	// defer log.Printf("select loop closed: %p", r)

	defer close(r.C)
	query, args, err := psql.
		Select("id", "data").
		From(r.table).
		Where(r.filter.Sqlizer).
		Offset(r.filter.Offset).
		OrderBy("id").
		ToSql()
	if err != nil {
		r.Err = err
		return
	}

	rows, err := r.julio.DB.Query(query, args...)
	if err != nil {
		r.Err = err
		return
	}

	for rows.Next() {
		row := Row{}
		err := rows.Scan(&row.ID, &row.Data)
		if err != nil {
			r.Err = err
			return
		}

		select {
		case r.C <- row:
		case <-r.done:
			return
		}
	}

	rows.Close()
	if rows.Err() != nil {
		r.Err = rows.Err()
		return
	}

	for row := range r.backlog {
		r.C <- row
	}
}

func (r *Rows) Close() {
	defer func() { recover() }()
	close(r.done)
}