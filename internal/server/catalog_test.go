package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jawalab-com/payrouter/internal/adapters/stub"
	"github.com/jawalab-com/payrouter/internal/config"
	"github.com/jawalab-com/payrouter/internal/server"
)

// TestContractCatalogDurableAndIsolated: durable catalog (customers/products/
// prices) is account-scoped — a row created by account A is 404 to account B,
// and survives a fresh server handle (restart).
func TestContractCatalogDurableAndIsolated(t *testing.T) {
	f := setupFacade(t)

	// Product.
	status, body := f.do(t, http.MethodPost, "/v1/products", "sk_test_aaa", "", "name=Widget&description=hi")
	if status != http.StatusOK {
		t.Fatalf("create product: %d %s", status, body)
	}
	prodID := extractID(body)
	if status, _ := f.do(t, http.MethodGet, "/v1/products/"+prodID, "sk_test_aaa", "", ""); status != http.StatusOK {
		t.Fatalf("owner retrieve product: %d", status)
	}
	if status, _ := f.do(t, http.MethodGet, "/v1/products/"+prodID, "sk_test_bbb", "", ""); status != http.StatusNotFound {
		t.Fatalf("cross-account product: %d want 404", status)
	}

	// Price on that product.
	status, body = f.do(t, http.MethodPost, "/v1/prices", "sk_test_aaa", "", "product="+prodID+"&currency=usd&unit_amount=1500")
	if status != http.StatusOK {
		t.Fatalf("create price: %d %s", status, body)
	}
	priceID := extractID(body)
	if status, _ := f.do(t, http.MethodGet, "/v1/prices/"+priceID, "sk_test_aaa", "", ""); status != http.StatusOK {
		t.Fatalf("owner retrieve price: %d", status)
	}
	if status, _ := f.do(t, http.MethodGet, "/v1/prices/"+priceID, "sk_test_bbb", "", ""); status != http.StatusNotFound {
		t.Fatalf("cross-account price: %d want 404", status)
	}

	// Customer.
	status, body = f.do(t, http.MethodPost, "/v1/customers", "sk_test_aaa", "", "email=a@x.test&name=A")
	if status != http.StatusOK {
		t.Fatalf("create customer: %d %s", status, body)
	}
	cusID := extractID(body)
	if status, _ := f.do(t, http.MethodGet, "/v1/customers/"+cusID, "sk_test_bbb", "", ""); status != http.StatusNotFound {
		t.Fatalf("cross-account customer: %d want 404", status)
	}

	// Restart persistence: a brand-new server handle against the same pool reads
	// account A's product.
	restored := server.New(config.Config{APIKey: "sk_test_aaa", ActiveGateway: "stub"}, f.pg, stub.New())
	rs := httptest.NewServer(restored)
	t.Cleanup(rs.Close)
	req, _ := http.NewRequest(http.MethodGet, rs.URL+"/v1/products/"+prodID, nil)
	req.Header.Set("Authorization", "Bearer sk_test_aaa")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retrieve product after restart: %d", resp.StatusCode)
	}
	resp.Body.Close()
}
