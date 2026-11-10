// fake-ruvds: fake RUVDS API v2 для e2e-стенда (add-environment-provisioning):
// каталог, цена, баланс, асинхронное создание, ожидание оплаты, удаление.
// Только тестовая инфраструктура: в образ rivetd и консоль не попадает.
// Любой непустой токен принимается, токен со словом invalid отклоняется
// 401, со словом lowbalance видит баланс 10 ₽ (сценарий нехватки денег).
// GET /__fake/calls отдаёт журнал запросов: e2e проверяет по нему, что без
// подтверждения создания и удаления не было.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/PavluninVladimir/rivet/internal/infra/ruvds/ruvdstest"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8285", "адрес HTTP")
	balance := flag.Float64("balance", 10000, "баланс по умолчанию, ₽")
	readyAfter := flag.Int("ready-after", 2, "сколько опросов идёт создание сервера")
	flag.Parse()
	f := ruvdstest.New()
	f.Balance, f.ReadyAfter = *balance, *readyAfter
	fmt.Println("fake-ruvds:", *addr)
	srv := &http.Server{Addr: *addr, Handler: f, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
