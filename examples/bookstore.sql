-- A small bookstore, generated with plain SQL: run it with
--   cairn shop.db < examples/bookstore.sql
-- then try the queries at the end.

CREATE TABLE digits (d INTEGER PRIMARY KEY);
INSERT INTO digits VALUES (0), (1), (2), (3), (4), (5), (6), (7), (8), (9);

CREATE TABLE authors (
  id      INTEGER PRIMARY KEY,
  name    TEXT NOT NULL,
  country TEXT
);

CREATE TABLE books (
  id        INTEGER PRIMARY KEY,
  title     TEXT NOT NULL,
  author_id INTEGER NOT NULL,
  year      INTEGER,
  price     REAL NOT NULL,
  genre     TEXT
);
CREATE INDEX books_author ON books (author_id);
CREATE INDEX books_genre_year ON books (genre, year);

CREATE TABLE customers (
  id    INTEGER PRIMARY KEY,
  email TEXT NOT NULL UNIQUE,
  city  TEXT
);

CREATE TABLE orders (
  id          INTEGER PRIMARY KEY,
  customer_id INTEGER NOT NULL,
  book_id     INTEGER NOT NULL,
  quantity    INTEGER NOT NULL DEFAULT 1,
  day         INTEGER NOT NULL
);
CREATE INDEX orders_customer ON orders (customer_id);
CREATE INDEX orders_book ON orders (book_id);

-- 100 authors
INSERT INTO authors (id, name, country)
SELECT a.d * 10 + b.d + 1,
       'Author ' || (a.d * 10 + b.d + 1),
       CASE (a.d * 10 + b.d) % 5 WHEN 0 THEN 'Norway' WHEN 1 THEN 'Chile' WHEN 2 THEN 'Japan' WHEN 3 THEN 'Kenya' ELSE 'Canada' END
FROM digits a, digits b;

-- 1,000 books
INSERT INTO books (id, title, author_id, year, price, genre)
SELECT n + 1,
       'Book ' || (n + 1),
       n % 100 + 1,
       1950 + (n * 7) % 75,
       round(5 + (n * 37 % 400) / 10.0, 2),
       CASE n % 4 WHEN 0 THEN 'fiction' WHEN 1 THEN 'science' WHEN 2 THEN 'history' ELSE 'poetry' END
FROM (SELECT a.d * 100 + b.d * 10 + c.d AS n FROM digits a, digits b, digits c) AS nums;

-- 1,000 customers
INSERT INTO customers (id, email, city)
SELECT n + 1, 'reader' || (n + 1) || '@example.com',
       CASE n % 3 WHEN 0 THEN 'Oslo' WHEN 1 THEN 'Santiago' ELSE 'Osaka' END
FROM (SELECT a.d * 100 + b.d * 10 + c.d AS n FROM digits a, digits b, digits c) AS nums;

-- 10,000 orders
INSERT INTO orders (customer_id, book_id, quantity, day)
SELECT (n * 7919) % 1000 + 1, (n * 104729) % 1000 + 1, 1 + n % 3, n % 365
FROM (SELECT a.d * 1000 + b.d * 100 + c.d * 10 + e.d AS n FROM digits a, digits b, digits c, digits e) AS nums;

-- Try these:
--
-- Best-selling genres:
--   SELECT b.genre, sum(o.quantity) AS sold, round(sum(o.quantity * b.price), 2) AS revenue
--   FROM orders o JOIN books b ON b.id = o.book_id
--   GROUP BY b.genre ORDER BY revenue DESC;
--
-- One customer's orders (index lookups all the way down):
--   EXPLAIN SELECT b.title, o.quantity FROM orders o JOIN books b ON b.id = o.book_id WHERE o.customer_id = 42;
--
-- Authors whose books average above the store's average price:
--   SELECT a.name, round(avg(b.price), 2) AS avg_price FROM authors a JOIN books b ON b.author_id = a.id
--   GROUP BY a.name HAVING avg(b.price) > (SELECT avg(price) FROM books) ORDER BY avg_price DESC LIMIT 5;
