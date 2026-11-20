package route

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/GMWalletApp/epusdt/model/dao"
	"github.com/GMWalletApp/epusdt/model/data"
	"github.com/GMWalletApp/epusdt/model/mdb"
	"github.com/GMWalletApp/epusdt/model/request"
	"github.com/GMWalletApp/epusdt/model/service"
	"github.com/labstack/echo/v4"
)

const epaySubmitPath = "/payments/epay/v1/order/create-transaction/submit.php"

func epayRepeatValues(orderID string) url.Values {
	return url.Values{
		"pid":          {"1"},
		"name":         {orderID},
		"type":         {"alipay"},
		"money":        {"1.00"},
		"out_trade_no": {orderID},
		"notify_url":   {"https://93.184.216.34/notify"},
		"return_url":   {"http://localhost/return"},
	}
}

func submitEpayRepeat(e *echo.Echo, values url.Values) *httptest.ResponseRecorder {
	signed := url.Values{}
	for key, items := range values {
		signed[key] = append([]string(nil), items...)
	}
	return doFormPost(e, epaySubmitPath, signEpayValues(signed))
}

func mustSubmitEpayCheckout(t *testing.T, e *echo.Echo, values url.Values) string {
	t.Helper()
	rec := submitEpayRepeat(e, values)
	location := rec.Header().Get("Location")
	if rec.Code != http.StatusFound || !strings.HasPrefix(location, "/pay/checkout-counter/") {
		t.Fatalf("expected checkout redirect, got %d location=%q body=%s", rec.Code, location, rec.Body.String())
	}
	return location
}

func assertEpayRepeatConflict(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("repeat expected 400, got %d location=%q body=%s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	if got := int(parseResp(t, rec)["status_code"].(float64)); got != 10002 {
		t.Fatalf("repeat status_code = %d, want 10002", got)
	}
}

// A repeated submit.php may only reopen the checkout for the same payment;
// otherwise the payer could be shown a stale amount, callback or channel.
func TestEpaySubmitPhpRepeatRejectsChangedRequest(t *testing.T) {
	e := setupTestEnv(t)
	for _, tc := range []struct {
		field string
		value string
	}{
		{"money", "2.00"},
		{"notify_url", "https://93.184.216.35/notify"},
		{"return_url", "http://localhost/other-return"},
		{"type", "usdt.tron"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			orderID := "epay-repeat-changed-" + strings.ReplaceAll(tc.field, "_", "-")
			mustSubmitEpayCheckout(t, e, epayRepeatValues(orderID))
			changed := epayRepeatValues(orderID)
			changed.Set(tc.field, tc.value)
			assertEpayRepeatConflict(t, submitEpayRepeat(e, changed))
		})
	}
}

// Paid or expired orders keep the original conflict; only orders that can
// still be paid are resumable.
func TestEpaySubmitPhpRepeatRejectsFinishedOrder(t *testing.T) {
	e := setupTestEnv(t)
	for _, status := range []int{mdb.StatusPaySuccess, mdb.StatusExpired} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			values := epayRepeatValues(fmt.Sprintf("epay-repeat-finished-%d", status))
			location := mustSubmitEpayCheckout(t, e, values)
			tradeID := strings.TrimPrefix(location, "/pay/checkout-counter/")
			if err := dao.Mdb.Model(&mdb.Orders{}).Where("trade_id = ?", tradeID).Update("status", status).Error; err != nil {
				t.Fatal(err)
			}
			assertEpayRepeatConflict(t, submitEpayRepeat(e, values))
		})
	}
}

// Merchants such as Sub2API reopen the original signed submit.php URL. That
// must keep working while the payer is choosing a network and after the
// checkout has completed the placeholder with the chosen network.
func TestEpaySubmitPhpRepeatResumesPlaceholderAfterNetworkSelection(t *testing.T) {
	e := setupTestEnv(t)
	if err := data.SetSetting(mdb.SettingGroupEpay, mdb.SettingKeyEpayDefaultToken, "", mdb.SettingTypeString); err != nil {
		t.Fatalf("clear epay.default_token: %v", err)
	}
	if err := data.SetSetting(mdb.SettingGroupEpay, mdb.SettingKeyEpayDefaultNetwork, "", mdb.SettingTypeString); err != nil {
		t.Fatalf("clear epay.default_network: %v", err)
	}

	values := epayRepeatValues("epay-repeat-placeholder")
	location := mustSubmitEpayCheckout(t, e, values)
	tradeID := strings.TrimPrefix(location, "/pay/checkout-counter/")
	order, err := data.GetOrderInfoByTradeId(tradeID)
	if err != nil || order.Status != mdb.StatusWaitSelect {
		t.Fatalf("expected placeholder order, got %+v err=%v", order, err)
	}
	if again := mustSubmitEpayCheckout(t, e, values); again != location {
		t.Fatalf("placeholder repeat redirect = %q, want %q", again, location)
	}

	if _, err := service.SwitchNetwork(&request.SwitchNetworkRequest{TradeId: tradeID, Token: "USDT", Network: mdb.NetworkTron}); err != nil {
		t.Fatalf("select network: %v", err)
	}
	order, err = data.GetOrderInfoByTradeId(tradeID)
	if err != nil || order.Status != mdb.StatusWaitPay || order.Network != mdb.NetworkTron {
		t.Fatalf("expected placeholder completed in place, got %+v err=%v", order, err)
	}
	if again := mustSubmitEpayCheckout(t, e, values); again != location {
		t.Fatalf("repeat after network selection redirect = %q, want %q", again, location)
	}
}
