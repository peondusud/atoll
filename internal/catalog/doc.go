// Package catalog holds the queries over the SQLite catalog (WAL,
// synchronous=FULL). The catalog is a rebuildable cache: the truth is on the
// disks. The schema lives in schema/migrations/.
package catalog
