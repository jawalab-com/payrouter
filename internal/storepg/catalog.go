package storepg

import (
	"context"
	"encoding/json"

	"github.com/stripe-compatible-facade/internal/store"
)

// Native account-scoped persistence for the catalog (customers, products, prices),
// migrated off the in-memory store in Phase 5D part 1. Put* use a background
// context (the Store interface carries none) and upsert by id; HTTP isolation is
// enforced by the handlers comparing the caller's account to the row's owner.

// --- Customers --------------------------------------------------------------

func (s *Store) PutCustomer(c *store.Customer) {
	meta, _ := json.Marshal(c.Metadata)
	_, _ = s.pool.Exec(context.Background(), `
		INSERT INTO customers (id,account_id,email,name,phone,metadata,created,livemode)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (id) DO UPDATE SET
			email=EXCLUDED.email, name=EXCLUDED.name, phone=EXCLUDED.phone,
			metadata=EXCLUDED.metadata, livemode=EXCLUDED.livemode`,
		c.ID, c.AccountID, c.Email, c.Name, c.Phone, meta, c.Created, c.Livemode)
}

func (s *Store) GetCustomer(id string) (*store.Customer, error) {
	row := s.pool.QueryRow(context.Background(),
		`SELECT id,account_id,email,name,phone,metadata,created,livemode FROM customers WHERE id=$1`, id)
	c, err := scanCustomer(row)
	return c, wrapNotFound(err)
}

func scanCustomer(row interface {
	Scan(dest ...any) error
}) (*store.Customer, error) {
	var c store.Customer
	var meta []byte
	if err := row.Scan(&c.ID, &c.AccountID, &c.Email, &c.Name, &c.Phone, &meta, &c.Created, &c.Livemode); err != nil {
		return nil, err
	}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &c.Metadata)
	}
	return &c, nil
}

// --- Products ---------------------------------------------------------------

func (s *Store) PutProduct(p *store.Product) {
	meta, _ := json.Marshal(p.Metadata)
	_, _ = s.pool.Exec(context.Background(), `
		INSERT INTO products (id,account_id,name,description,active,metadata,created,updated,livemode)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (id) DO UPDATE SET
			name=EXCLUDED.name, description=EXCLUDED.description, active=EXCLUDED.active,
			metadata=EXCLUDED.metadata, updated=EXCLUDED.updated, livemode=EXCLUDED.livemode`,
		p.ID, p.AccountID, p.Name, p.Description, p.Active, meta, p.Created, p.Updated, p.Livemode)
}

func (s *Store) GetProduct(id string) (*store.Product, error) {
	row := s.pool.QueryRow(context.Background(),
		`SELECT id,account_id,name,description,active,metadata,created,updated,livemode FROM products WHERE id=$1`, id)
	p, err := scanProduct(row)
	return p, wrapNotFound(err)
}

func scanProduct(row interface {
	Scan(dest ...any) error
}) (*store.Product, error) {
	var p store.Product
	var meta []byte
	if err := row.Scan(&p.ID, &p.AccountID, &p.Name, &p.Description, &p.Active, &meta, &p.Created, &p.Updated, &p.Livemode); err != nil {
		return nil, err
	}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &p.Metadata)
	}
	return &p, nil
}

// --- Prices -----------------------------------------------------------------

func (s *Store) PutPrice(p *store.Price) {
	meta, _ := json.Marshal(p.Metadata)
	_, _ = s.pool.Exec(context.Background(), `
		INSERT INTO prices (id,account_id,product_id,unit_amount,currency,type,interval,interval_count,usage_type,active,metadata,created,livemode)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (id) DO UPDATE SET
			product_id=EXCLUDED.product_id, unit_amount=EXCLUDED.unit_amount, currency=EXCLUDED.currency,
			type=EXCLUDED.type, interval=EXCLUDED.interval, interval_count=EXCLUDED.interval_count,
			usage_type=EXCLUDED.usage_type, active=EXCLUDED.active, metadata=EXCLUDED.metadata, livemode=EXCLUDED.livemode`,
		p.ID, p.AccountID, p.ProductID, p.UnitAmount, p.Currency, p.Type, p.Interval, p.IntervalCount,
		p.UsageType, p.Active, meta, p.Created, p.Livemode)
}

func (s *Store) GetPrice(id string) (*store.Price, error) {
	row := s.pool.QueryRow(context.Background(),
		`SELECT id,account_id,product_id,unit_amount,currency,type,interval,interval_count,usage_type,active,metadata,created,livemode FROM prices WHERE id=$1`, id)
	p, err := scanPrice(row)
	return p, wrapNotFound(err)
}

func scanPrice(row interface {
	Scan(dest ...any) error
}) (*store.Price, error) {
	var p store.Price
	var meta []byte
	if err := row.Scan(&p.ID, &p.AccountID, &p.ProductID, &p.UnitAmount, &p.Currency, &p.Type,
		&p.Interval, &p.IntervalCount, &p.UsageType, &p.Active, &meta, &p.Created, &p.Livemode); err != nil {
		return nil, err
	}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &p.Metadata)
	}
	return &p, nil
}
