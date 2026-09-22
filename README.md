# HDFC to Firefly III

This tool reads HDFC Bank alert emails from Gmail. It writes each transaction
to Firefly III.

## What the tool does

The tool reads the mailbox on a schedule. It finds the HDFC alert emails. It
reads these values from each email:

- The amount
- The direction of the money
- The account
- The payee
- The UPI reference number

Then the tool writes one transaction to Firefly III.

The tool writes each transaction one time only. It keeps a record of each email
that it read. It also asks Firefly III if the transaction is there already.

The tool does not read card alerts. No example of a card alert is available. A
card alert goes to the `skipped` list. To add a new format, see "Add a new
email format".

## Requirements

- A Gmail account with 2-Step Verification
- A Gmail app password
- A Firefly III personal access token
- A computer that is always on, or Docker

## Setup

### Step 1: Make a Gmail app password

Gmail does not agree to your usual password from a program. You must make an
app password of 16 characters.

1. Open <https://myaccount.google.com/security>.
2. Turn on 2-Step Verification.
3. Open <https://myaccount.google.com/apppasswords>.
4. Make a new app password. Give it the name `hdfc2ff`.
5. Copy the 16 characters.
6. Remove the spaces from the password.

**Note:** An app password has no expiry date. It gives access to your email
only. You can delete the app password at any time.

### Step 2: Make a Firefly III token

1. Open Firefly III.
2. Go to **Options > Profile > OAuth**.
3. Click **Create new token**.
4. Copy the token. The token goes out one time only.

### Step 3: Find the account IDs

The tool must know which Firefly III account agrees with each HDFC account. The
tool uses the last four digits of the HDFC account.

1. Open the account in Firefly III.
2. Look at the URL. The URL `/accounts/show/3` gives the ID `3`.

### Step 4: Write the configuration

1. Copy the example file.

   ```sh
   cp .env.example .env
   ```

2. Open `.env` in an editor.
3. Fill in these values:

   - `GMAIL_ADDRESS`
   - `GMAIL_APP_PASSWORD`
   - `FIREFLY_URL`
   - `FIREFLY_TOKEN`
   - `ACCOUNT_MAP`

`ACCOUNT_MAP` uses the format `last4=fireflyAccountID`. For two accounts, write
`ACCOUNT_MAP=1234=3,5678=9`.

### Step 5: Do a test run

Do a test run first. A test run writes nothing to Firefly III.

```sh
make build
./hdfc2ff -dry-run -verbose once
```

Read the output. Make sure that the amounts, the payees, and the account digits
are correct.

### Step 6: Run the tool

```sh
./hdfc2ff once     # poll one time, then stop
./hdfc2ff run      # continue to poll
```

## Commands

| Command | Function |
| --- | --- |
| `hdfc2ff run` | Continue to poll Gmail |
| `hdfc2ff once` | Poll one time, then stop |
| `hdfc2ff status` | Show the results |
| `hdfc2ff dump` | Show how the tool reads the messages |
| `hdfc2ff import` | Import one message by UID |
| `hdfc2ff verify` | Compare the ledger with the bank |
| `hdfc2ff healthcheck` | Find out if the polls operate correctly |
| `hdfc2ff replay` | Try the failed messages again |

These flags are also available:

| Flag | Function |
| --- | --- |
| `-dry-run` | Read the messages, but write nothing |
| `-verbose` | Show more log messages |
| `-env <path>` | Use a different configuration file |
| `-limit <n>` | Limit the number of records |
| `-save <dir>` | Save real emails as test files |
| `-status <value>` | Select `failed`, `skipped`, or `all` for `replay` |
| `-uid <list>` | Select message UIDs for `import` |
| `-lookback <duration>` | Set the search range for `verify` |
| `-max-age <duration>` | Set the age limit for `healthcheck` |

## Run the tool as a service

### With systemd

Use the long-running service. systemd starts the tool again after a failure.

```sh
sudo cp deploy/systemd/hdfc2ff.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now hdfc2ff.service
journalctl -u hdfc2ff -f
```

Change `User` and `WorkingDirectory` in the unit file before you start it.

As an alternative, use the timer. The timer starts the tool one time every five
minutes.

```sh
sudo cp deploy/systemd/hdfc2ff-once.service deploy/systemd/hdfc2ff-once.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now hdfc2ff-once.timer
```

Enable one of the two only. Two tools that operate at the same time cause no
double transactions, because the tool claims each message before it starts
work. But two tools are not necessary.

### With Docker

```sh
docker compose up -d
docker compose logs -f
```

The image has no shell. The size is approximately 25 MB. Docker reads the
credentials from `.env`. Docker keeps the state on the `hdfc2ff-data` volume.

**CAUTION:** Do not start the container with a new, empty volume together with
your live Firefly III. The container will write transactions again. The local
database holds the record of the messages that the tool must not import. An
empty volume has no such record. Copy the database to the volume before the
first start.

## How the tool operates

The tool continues to operate after an error.

- An error in one poll stops that poll only. The tool tries again. After
  failures, the tool waits longer each time. The maximum wait is 30 minutes.
- An error in one message does not stop the other messages.
- A `SIGTERM` signal stops the tool after the current message. A second signal
  stops the tool immediately.
- A slow server does not hang the tool. Each poll has a time limit of 90
  seconds.
- After a power failure, the tool continues with the next start. It tries the
  interrupted messages again.
- The `healthcheck` command gives a correct result. It reads the time of the
  last good poll.

  ```sh
  hdfc2ff healthcheck -max-age 15m
  ```

  The command gives a zero exit code if the poll is recent. It gives a non-zero
  exit code if the poll is too old. The Docker healthcheck uses this command.

## Check the ledger with the bank

The bank sends balance notices. A balance notice gives the true balance of the
account:

```
Available balance in your account ending XX1234 is Rs. INR 12,345.67 as on 15-JAN-26.
```

The `verify` command finds the most recent balance notice. It calculates the
balance from the ledger. Then it compares the two values.

```sh
hdfc2ff verify
```

```
Account ending 1234, as on 15 Jan 2026
  bank says:   12345.67
  ledger says: 12345.67
  difference:  0.00

The ledger matches the bank.
```

The command gives a non-zero exit code if the values are different. Thus you
can use the command as a check.

A balance notice is never imported as a transaction.

## Add a new email format

The tool reads each email with a table of patterns. If a message agrees with no
pattern, the tool writes it to the `skipped` list.

1. Save the email as a test file.

   ```sh
   hdfc2ff dump -save testdata/samples
   ```

2. Run the tests.

   ```sh
   make samples
   ```

3. Read the output. If the result is not correct, add a pattern to
   `internal/hdfcmail/parse.go`.
4. Save the correct result as a `.json` file. Give the file the same name as
   the `.eml` file.

The test files contain your account digits and the names of the payees. They do
not go into the repository. See `testdata/samples/README.md`.

## Correct a difference

If `verify` shows a difference, one transaction is absent from the ledger or
one transaction is too many.

1. Run `hdfc2ff dump` with a large `LOOKBACK` value.
2. Compare the emails with the ledger.
3. If the email is older than the lookback range, import the message again.

   ```sh
   hdfc2ff import -uid 1259
   ```

If a transaction is in Firefly III two times, keep one transaction and delete
the other one in the Firefly III interface. Then mark the message as `ignored`:

```sh
sqlite3 hdfc2ff.db "
  UPDATE messages
  SET status = 'ignored',
      detail = 'the hand-entered transaction is kept'
  WHERE firefly_id = '37';
"
```

The `ignored` status keeps the message out of `replay`. Without this status,
the tool can import the message again.

## Problems

**The message `Application-specific password required` appears.**
Use the app password from Step 1, not your Google password.

**The message `missing required setting(s): ACCOUNT_MAP` appears.**
Fill in `ACCOUNT_MAP` in `.env`. See Step 3.

**A message goes to `skipped` with `no account mapped for last4`.**
An alert arrived for an account that is not in `ACCOUNT_MAP`. Add the account.
Run `hdfc2ff status` to see these messages.

**A message goes to `skipped` with `not a transaction alert`.**
This result is correct for statements and for balance notices. If a true
transaction goes to this list, the tool does not know the format. Save the
email and add a pattern. See "Add a new email format".

**The message `firefly: token rejected` appears.**
Make a new personal access token. Make sure that `FIREFLY_URL` gives the root of
the instance and not `/api/v1`.

**The tool finds no messages.**
Make sure that `GMAIL_FROM` agrees with the sender. Make sure that `LOOKBACK`
covers the necessary period. If a Gmail filter moves the alerts, set
`GMAIL_MAILBOX` to the name of the label.

## Development

```sh
make test     # unit tests and the sample tests
make build    # make the binary hdfc2ff
make check    # go vet and the tests
make samples  # show the results for the real emails
```

The build uses Go only and no cgo. The result is one static binary.

The amount values use integer minor units. One rupee is 100 minor units. No
floating-point value touches the money path.

### DNS in this environment

On one development machine, the Go resolver selected a bad address for
`proxy.golang.org`. The download of the modules failed with `no route to
host`. The system resolver corrected the problem:

```sh
export GODEBUG=netdns=cgo
```

This command is necessary on that machine only.
