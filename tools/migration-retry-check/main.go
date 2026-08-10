package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/jmoiron/sqlx"

	"github.com/fleetdm/fleet/v4/server/datastore/mysql/migrations/tables"
	platform_mysql "github.com/fleetdm/fleet/v4/server/platform/mysql"
)

const childProcessEnvVar = "MIGRATION_RETRY_CHECK_CHILD"

func main() {
	migrations := flag.String("migrations", "", "migration timestamps or file paths to check, separated by commas or whitespace")
	flag.Parse()

	if childUsername := os.Getenv(childProcessEnvVar); childUsername != "" {
		database, err := openDatabase(childUsername, "migration_retry_check")
		if err != nil {
			log.Fatal(err)
		}
		if err := tables.MigrationClient.UpByOne(database.DB, ""); err != nil {
			log.Fatal(err)
		}
		return
	}

	if *migrations == "" {
		log.Fatal("no migrations given, pass -migrations=20260608160653[,...]")
	}

	var migrationVersions []int64
	for _, field := range strings.FieldsFunc(*migrations, isMigrationSeparator) {
		timestamp, _, _ := strings.Cut(filepath.Base(field), "_")
		version, err := strconv.ParseInt(timestamp, 10, 64)
		if err != nil {
			log.Fatalf("parsing migration timestamp from %q: %s", field, err)
		}
		migrationVersions = append(migrationVersions, version)
	}

	for _, migrationVersion := range migrationVersions {
		if err := checkMigrationIsRetryable(migrationVersion); err != nil {
			log.Fatal(err)
		}
	}
}

func isMigrationSeparator(r rune) bool {
	return r == ',' || unicode.IsSpace(r)
}

func checkMigrationIsRetryable(migrationVersion int64) error {
	server, err := openDatabase("root", "")
	if err != nil {
		return err
	}
	defer server.Close()

	if _, err := server.Exec(`
DROP DATABASE IF EXISTS migration_retry_check;
CREATE DATABASE migration_retry_check`); err != nil {
		return err
	}

	if err := applyMigrationsBeforeVersion(migrationVersion); err != nil {
		return err
	}

	// The grant on the version table has to run after the migrations that create it.
	grants := fmt.Sprintf(`
DROP USER IF EXISTS 'ddlonly'@'%%';
CREATE USER 'ddlonly'@'%%' IDENTIFIED BY 'toor';
GRANT ALL PRIVILEGES ON migration_retry_check.* TO 'ddlonly'@'%%';
REVOKE INSERT, UPDATE, DELETE ON migration_retry_check.* FROM 'ddlonly'@'%%';
GRANT INSERT ON migration_retry_check.%s TO 'ddlonly'@'%%'`, tables.MigrationClient.TableName)

	if _, err := server.Exec(grants); err != nil {
		return err
	}

	if applyMigrationInSubprocess("ddlonly") == 0 {
		fmt.Printf("%d: issues no DML, not applicable\n", migrationVersion)
		return nil
	}

	if applyMigrationInSubprocess("root") != 0 {
		return fmt.Errorf("%d is not retryable after a mid-migration failure\n"+
			"guard DDL past the first statement with the ...Exists() helpers in migration.go", migrationVersion)
	}

	return nil
}

func applyMigrationsBeforeVersion(migrationVersion int64) error {
	database, err := openDatabase("root", "migration_retry_check")
	if err != nil {
		return err
	}
	defer database.Close()

	for {
		currentVersion, err := tables.MigrationClient.GetDBVersion(database.DB)
		if err != nil {
			return err
		}

		nextMigration, err := tables.MigrationClient.Migrations.Next(currentVersion)
		if err != nil {
			return err
		}

		if nextMigration.Version == migrationVersion {
			return nil
		}

		if err := tables.MigrationClient.UpByOne(database.DB, ""); err != nil {
			return err
		}
	}
}

func applyMigrationInSubprocess(username string) int {
	executablePath, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}

	command := exec.Command(executablePath)
	command.Env = append(os.Environ(), childProcessEnvVar+"="+username)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr

	err = command.Run()
	if err == nil {
		return 0
	}

	if exitError, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitError.ExitCode()
	}

	log.Fatal(err)
	return 1
}

func openDatabase(username string, database string) (*sqlx.DB, error) {
	address := "localhost:3307"
	if port := os.Getenv("FLEET_MYSQL_TEST_PORT"); port != "" {
		address = "localhost:" + port
	}

	config := platform_mysql.MysqlConfig{
		Protocol:     "tcp",
		Address:      address,
		Username:     username,
		Password:     "toor",
		Database:     database,
		MaxOpenConns: 5,
		MaxIdleConns: 5,
	}

	return platform_mysql.NewDB(&config, &platform_mysql.DBOptions{
		MaxAttempts: 5,
		Logger:      slog.Default(),
	}, "")
}
