/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

package main

import (
	"database/sql"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/go-gorp/gorp"
	"github.com/jcjones/ct-sql/sqldb"
	"github.com/jcjones/ct-sql/utils"
	"github.com/oschwald/geoip2-golang"
)

type ResolutionEntry struct {
	NameID uint64
	Name   string
	Time   *time.Time
	Ipaddr *string
}

var (
	config = utils.NewCTConfig()
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("")
	dbConnectStr, err := sqldb.RecombineURLForDB(*config.DbConnect)
	if err != nil {
		log.Printf("unable to parse %s: %s", *config.DbConnect, err)
	}

	if len(dbConnectStr) == 0 || len(*config.GeoipDbPath) == 0 {
		config.Usage()
		os.Exit(2)
	}

	db, err := sql.Open("mysql", dbConnectStr)
	if err != nil {
		log.Fatalf("unable to open SQL: %s: %s", dbConnectStr, err)
	}
	if err = db.Ping(); err != nil {
		log.Fatalf("unable to ping SQL: %s: %s", dbConnectStr, err)
	}

	dialect := gorp.MySQLDialect{Engine: "InnoDB", Encoding: "UTF8"}
	dbMap := &gorp.DbMap{Db: db, Dialect: dialect}
	entriesDb := &sqldb.EntriesDatabase{
		DbMap:        dbMap,
		SQLDebug:     *config.SQLDebug,
		Verbose:      *config.Verbose,
		KnownIssuers: make(map[string]int),
	}
	err = entriesDb.InitTables()
	if err != nil {
		log.Fatalf("unable to prepare SQL DB. dbConnectStr=%s: %s", dbConnectStr, err)
	}

	geoDB, err := geoip2.Open(*config.GeoipDbPath)
	if err != nil {
		log.Fatalf("unable to prepare GeoIP DB. geoipDbPath=%s: %s", *config.GeoipDbPath, err)
	}
	defer geoDB.Close()

	netscan := &NetScan{
		wg:    new(sync.WaitGroup),
		db:    entriesDb,
		geodb: geoDB,
	}

	if *config.Limit == 0 {
		// Didn't include a mandatory action, so print usage and exit.
		log.Fatalf("You must set a limit")
	}

	oldestAllowed := time.Now().AddDate(-1, 0, 0)

	var entries []ResolutionEntry
	_, err = dbMap.Select(&entries,
		`SELECT q.nameID, f.name FROM netscanqueue AS q
        NATURAL JOIN fqdn AS f
        LIMIT :limit`,
		map[string]interface{}{
			"oldestAllowed": oldestAllowed,
			"limit":         *config.Limit,
		})

	if err != nil {
		log.Fatalf("unable to execute SQL: %s", err)
	}

	err = netscan.processEntries(entries)

	if err != nil {
		log.Fatalf("error while running importer: %s", err)
	}

	netscan.wg.Wait()
	os.Exit(0)
}

type NetScan struct {
	db    *sqldb.EntriesDatabase
	wg    *sync.WaitGroup
	geodb *geoip2.Reader
}

func (ns *NetScan) resolveWorker(entries <-chan ResolutionEntry) {
	ns.wg.Add(1)
	defer ns.wg.Done()
	for e := range entries {
		// Unqueue the nameID
		err := ns.db.UnqueueFromNetscan(e.NameID)
		if err != nil {
			log.Printf("Could not dequeue host %s (ID=%d): %s", e.Name, e.NameID, err)
			continue
		}

		if strings.Contains(e.Name, "*") {
			// Is a wildcard, we can't resolve it.
			ns.db.InsertResolvedName(e.NameID, "")
			continue
		}

		ips, err := net.LookupIP(e.Name)
		if err != nil {
			if *config.Verbose {
				log.Printf("Could not lookup host %s: %s", e.Name, err)
			}

			// Insert a blank record so we know this one didn't work in the future
			ns.db.InsertResolvedName(e.NameID, "")
			// Can't proceed with the geo work since the IP didn't resolve
			continue
		}

		// Log each resolved IP
		for _, ip := range ips {
			ns.db.InsertResolvedName(e.NameID, ip.String())
		}

		// Look up the geo-ip data for the first resolved IP
		geoRecord, err := ns.geodb.City(ips[0])
		if err != nil {
			if *config.Verbose {
				log.Printf("Could not lookup geo-ip record for host %s: %s", e.Name, err)
			}
			continue
		}

		// Log the geo-ip data
		ns.db.InsertResolvedPlace(e.NameID, geoRecord.City.Names["en"],
			geoRecord.Country.IsoCode, geoRecord.Continent.Names["en"])
	}
}

func (ns *NetScan) processEntries(entries []ResolutionEntry) error {
	entryChan := make(chan ResolutionEntry, 10)
	defer close(entryChan)
	ns.wg.Add(1)
	defer ns.wg.Done()
	progressDisplay := utils.NewProgressDisplay()
	defer progressDisplay.Close()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(sigChan)
	defer close(sigChan)

	progressDisplay.StartDisplay(ns.wg)

	numWorkers := *config.NumThreads * runtime.NumCPU()
	for i := 0; i < numWorkers; i++ {
		go ns.resolveWorker(entryChan)
	}

	for i, entry := range entries {
		select {
		case entryChan <- entry:
			if i%256 == 0 {
				progressDisplay.UpdateProgress("Scanner", 0, uint64(i), uint64(len(entries)))
			}
		case sig := <-sigChan:
			return fmt.Errorf("Signal caught: %s", sig)
		}
	}

	return nil
}
